package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sort"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/memory"
	"github.com/tycheg/reasonr-engine/internal/config"
	"github.com/tycheg/reasonr-engine/internal/dispatch"
	"github.com/tycheg/reasonr-engine/internal/features"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/internal/pii"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/internal/registry"
	"github.com/tycheg/reasonr-engine/internal/rows"
	"github.com/tycheg/reasonr-engine/internal/transfer"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// hostOf is the host an engine kind gets (port.Host): the engine built so
// far and its configuration.
type hostOf struct {
	e       *Engine
	cfg     *config.Config
	opts    Options
	metrics port.Metrics
	kc      kernel.Config
}

var _ kind.Host = (*hostOf)(nil)

func (h *hostOf) Config() *kind.Config { return PortConfig(h.cfg) }

// PortConfig is the part of engine.yaml an engine kind reads (port.Config).
func PortConfig(c *config.Config) *kind.Config {
	return &kind.Config{Engine: c.Engine, Settings: c.Settings, Stores: c.Stores, State: c.State, Bus: c.Bus, Contexts: c.Contexts,
		Tenants: c.Tenants, Limits: kind.Limits(c.Limits), Projections: c.Projections}
}
func (h *hostOf) Settings() *port.Node                   { return h.cfg.Settings }
func (h *hostOf) Test() bool                             { return h.opts.Test }
func (h *hostOf) Log() *slog.Logger                      { return h.e.Log }
func (h *hostOf) Metrics() port.Metrics                  { return h.metrics }
func (h *hostOf) Models() map[string]reasonr.Interpreter { return h.opts.Models }
func (h *hostOf) Programs() kind.Programs                { return h.e.Programs }
func (h *hostOf) Store(name string) any                  { return h.e.Stores[name] }
func (h *hostOf) State() kind.UnitStore                  { return h.e.Store }
func (h *hostOf) Timers() kind.TimerSource               { return h.e.Timers }
func (h *hostOf) Hub() kind.NotificationHub              { return h.e.Hub }
func (h *hostOf) NewPrograms() kind.Programs             { return kernel.NewPrograms() }
func (h *hostOf) OnRequests(f func())                    { h.e.wakers = append(h.e.wakers, f) }

func (h *hostOf) TimerTenants() ([]port.Tenant, bool) {
	e := h.e
	if e.test == nil {
		return nil, true
	}
	var out []port.Tenant
	for _, t := range e.Programs.Tenants() {
		if !e.test.Ephemeral(t) {
			out = append(out, t)
		}
	}
	return out, false
}

func (h *hostOf) Coordinator() port.Coordinator    { return h.e.coordinator() }
func (h *hostOf) NewCoordinator() port.Coordinator { return memory.NewCoordinator() }
func (h *hostOf) SetTestEngine(x kind.TestEngine)  { h.e.SetTestEngine(x) }
func (h *hostOf) AddCloser(c port.Closer)          { h.e.AddCloser(c) }

func (h *hostOf) KernelConfig() kind.KernelConfig {
	kc := h.kc
	return kind.KernelConfig{Programs: kc.Programs, Store: kc.Store, Async: kc.Async, Timers: kc.Timers, Hub: kc.Hub, Clock: kc.Clock,
		UUID: kc.UUID, Retries: kc.Retries, ConsistencyWait: kc.ConsistencyWait, IdempotencyTTL: kc.IdempotencyTTL, Contexts: kc.Contexts,
		OnRequests: kc.OnRequests, Notify: kc.Notify, Logger: kc.Logger, Metrics: kc.Metrics, Features: kc.Features}
}

func (h *hostOf) NewKernel(c kind.KernelConfig) (kind.Kernel, error) { return NewKernel(c) }

// NewKernel is a kernel by a kind's configuration (port.Host.NewKernel).
func NewKernel(c kind.KernelConfig) (kind.Kernel, error) {
	return kernel.New(kernel.Config{Programs: c.Programs, Store: c.Store, Async: c.Async, Timers: c.Timers, Hub: c.Hub, Clock: c.Clock,
		UUID: c.UUID, Retries: c.Retries, ConsistencyWait: c.ConsistencyWait, IdempotencyTTL: c.IdempotencyTTL, Contexts: c.Contexts,
		OnRequests: c.OnRequests, Notify: c.Notify, Logger: c.Logger, Metrics: c.Metrics, Features: c.Features})
}

func (h *hostOf) NewMemoryStore(catalog kind.Catalog, o kind.MemoryOptions) kind.MemoryStore {
	kms := o.KMS
	if kms == nil {
		kms = pii.NewMemoryKMS()
	}
	return memory.NewStoreKeys(catalog, kms, pii.Options{Subjects: o.Subjects, Now: o.Now, Rand: o.Rand})
}

func (h *hostOf) NewFeatures(store port.FeatureStore) kind.FeatureResolver {
	return features.New(store, nil, h.e.Log)
}

func (h *hostOf) LocalKMS(key []byte, rand io.Reader) (port.KMS, error) {
	k, err := pii.NewLocalKMS(key)
	if err != nil {
		return nil, err
	}
	if rand != nil {
		return k.WithRand(rand), nil
	}
	return k, nil
}

func (h *hostOf) ACL(contexts []string, catalog kind.Catalog, run kind.DeliverFunc) kind.Consumer {
	return &dispatch.ACL{Contexts: contexts, Catalog: catalog, Run: run}
}

func (h *hostOf) ExportEntities(ctx context.Context, src kind.Reader, s *reasonr.Schema, tenant port.Tenant,
	each func(a *reasonr.Arena, rows []kind.EntityState) error) error {
	return transfer.Entities(ctx, src, s, tenant, each)
}

func (h *hostOf) ImportState(ctx context.Context, dst kind.UnitStore, tenant port.Tenant, a *reasonr.Arena, st kind.EngineState, now time.Time) error {
	return transfer.ImportState(ctx, dst, tenant, a, st, now)
}

// Effects builds the consumers of an effect stream of a kind: the
// projections of the configuration (or of c.Projections), the event store,
// the forget journal, the kind's own and the request sender.
func (h *hostOf) Effects(c kind.EffectsConfig) (*kind.Effects, error) {
	e, cfg := h.e, h.cfg
	byStore := c.Projections
	if byStore == nil {
		var err error
		if _, byStore, err = placeProjections(cfg, e.Programs, e.Stores); err != nil {
			return nil, err
		}
	}
	var consumers []dispatch.Consumer
	stores := make([]string, 0, len(byStore))
	for st := range byStore {
		stores = append(stores, st)
	}
	sort.Strings(stores)
	for _, st := range stores {
		ps, ok := e.Stores[st].(kind.ProjectionStore)
		if !ok {
			return nil, fmt.Errorf("store %q does not keep projections", st)
		}
		consumers = append(consumers, &dispatch.Projections{ConsumerName: "projections:" + st, Store: ps, Catalog: c.Catalog, Names: byStore[st]})
	}
	for _, k := range c.Consumers {
		consumers = append(consumers, k)
	}
	if cfg.Events != "" {
		if sink, ok := e.Stores[cfg.Events].(ports.EventStore); ok {
			consumers = append(consumers, &dispatch.Events{Sink: sink, OnAppend: e.busWake})
		}
	}
	if cfg.Crypto.ForgetLog != "" {
		if log, ok := rows.As[port.ForgetLog](e.Stores[cfg.Crypto.ForgetLog]); ok {
			consumers = append(consumers, &dispatch.Forgets{Log: log, Catalog: c.Catalog})
		}
	}
	out := &kind.Effects{}
	var sender *dispatch.Sender
	if old := e.Requests; old != nil && c.Requests != nil {
		sender = &dispatch.Sender{Queue: c.Requests, Default: old.Default, Systems: old.Systems, Catalog: c.Catalog,
			Workers: old.Workers, Retry: old.Retry, Log: old.Log, Metrics: old.Metrics}
		// The fake transport of test mode also runs the fake calls.
		transports := []ports.Transport{old.Default}
		names := make([]string, 0, len(old.Systems))
		for n := range old.Systems {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			transports = append(transports, old.Systems[n])
		}
		for _, t := range transports {
			if k, ok := t.(dispatch.Consumer); ok && !slices.Contains(consumers, k) {
				consumers = append(consumers, k)
			}
		}
		out.Wake = sender.Wake
	}
	for _, k := range consumers {
		out.Names = append(out.Names, k.Name())
	}
	log := c.Logger
	if log == nil {
		log = e.Log
	}
	out.Run = func(coord port.Coordinator) func(ctx context.Context) error {
		d := &dispatch.Dispatcher{Effects: c.Source, Coord: coord, Consumers: consumers, Log: log, Metrics: h.metrics}
		return func(ctx context.Context) error {
			switch {
			case len(consumers) == 0 && sender == nil:
				<-ctx.Done()
				return nil
			case len(consumers) == 0:
				return sender.Run(ctx)
			case sender == nil:
				return d.Run(ctx)
			}
			errs := make(chan error, 2)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			go func() { errs <- d.Run(ctx) }()
			go func() { errs <- sender.Run(ctx) }()
			err := <-errs
			cancel()
			return errors.Join(err, <-errs)
		}
	}
	out.Retry = func(coord port.Coordinator) func(ctx context.Context, consumer string) (int, error) {
		d := &dispatch.Dispatcher{Effects: c.Source, Coord: coord, Consumers: consumers, Log: log, Metrics: h.metrics}
		return d.Retry
	}
	return out, nil
}

// Tool adjusts the configuration of a tool process for its engine kind
// (port.KindTool); nil — the kind needs nothing.
func Tool(cfg *config.Config) error {
	k, err := registry.Kind(cfg.Engine)
	if err != nil {
		return err
	}
	if t, ok := k.(kind.KindTool); ok {
		return t.Tool(PortConfig(cfg)) // the kind changes its section (a shared node)
	}
	return nil
}

// HasTool reports whether the engine kind adjusts tool processes.
func HasTool(cfg *config.Config) bool {
	k, err := registry.Kind(cfg.Engine)
	_, ok := k.(kind.KindTool)
	return err == nil && ok
}

// TestRoles are the host roles the tests of the engine kind run next to it
// (port.KindTestRoles); nil — the kind runs its own.
func TestRoles(cfg *config.Config) []string {
	k, err := registry.Kind(cfg.Engine)
	if t, ok := k.(kind.KindTestRoles); err == nil && ok {
		return t.TestRoles()
	}
	return nil
}
