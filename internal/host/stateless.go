// Package stateless is the topology of the stateless engine (RFC §30–31):
// building the kernel and adapters from engine.yaml through the registry
// and running the process roles — api (incoming transports), timers (fired
// timers), dispatch (the effect stream and sending external requests).
// There may be many instances: leases in the main store share the work.
package host

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/auth"
	"github.com/tycheg/reasonr-engine/internal/adapters/memory"
	"github.com/tycheg/reasonr-engine/internal/adapters/telemetry"
	"github.com/tycheg/reasonr-engine/internal/bus"
	"github.com/tycheg/reasonr-engine/internal/config"
	"github.com/tycheg/reasonr-engine/internal/dispatch"
	"github.com/tycheg/reasonr-engine/internal/features"
	"github.com/tycheg/reasonr-engine/internal/hub"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/internal/pii"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/internal/registry"
	"github.com/tycheg/reasonr-engine/internal/rows"
	"github.com/tycheg/reasonr-engine/internal/testapi"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Roles are the host's process roles: api (the ingress) and dispatch (the
// bus from the event store); the engine kind adds its own (Engine.Roles).
var Roles = []string{"api", "dispatch"}

// Engine is a built engine.
type Engine struct {
	kind      kind.Engine // the engine kind opened on this host
	kindName  string
	host      *hostOf
	Programs  *kernel.Programs
	Store     kind.UnitStore
	Timers    kind.TimerSource // nil — the main store cannot keep timers
	Listeners []ports.Listener
	Edge      ports.EdgeDeps
	Log       *slog.Logger
	// MetricsAddr is a separate /metrics address; "" — on the HTTP transport.
	MetricsAddr string
	Prometheus  *telemetry.Prometheus
	// Requests are the transports of external requests and the send
	// settings: the effect stream of a kind sends with them (Host.Effects);
	// nil — the main store does not keep external requests.
	Requests *dispatch.Sender
	// wakers wake the request senders of the kind after a unit of work
	// with external requests (Host.OnRequests); coord is the coordinator
	// of a main store without one of its own, made once: the roles and
	// the kind ask for it from different goroutines.
	wakers    []func()
	coord     port.Coordinator
	coordOnce sync.Once
	// busWake wakes the bus after the event store appends (nil — no bus).
	busWake func()
	// Bus publishes events from the event store (the dispatch role); nil —
	// no bus.
	Bus *dispatch.Bus
	// Notifier publishes notifications to the bus without guarantees
	// (bus.notifications; it works with any roles: notifications appear
	// where commands run); nil — not published.
	Notifier *dispatch.Notifier
	// Topics are the broker topics of the bus and request transports:
	// EnsureTopics checks and creates them.
	Topics []Topic
	// Stores are the stores by name from engine.yaml.
	Stores map[string]any
	// KMS is the personal data master key (crypto.kms); nil — not set.
	KMS port.KMS
	// Features is the feature flag rollout (the features store, RFC §23.4).
	Features *features.Resolver
	// Hub is the notification hub (in test mode — with the journal of
	// ephemeral tenants).
	Hub kind.NotificationHub
	// Executor is the executor of the engine kind: the transports' calls
	// run on it (stateless: a kernel over the main store; a kind that runs
	// elsewhere: its client); nil — the process serves no API.
	Executor kind.Executor
	closers  []port.Closer
	test     *testapi.API // test mode; nil — production
}

// SetTestEngine sets the engine of the test API (/_test/*): the test engine
// of a kind that runs elsewhere instead of the kernel.
func (e *Engine) SetTestEngine(x testapi.Engine) {
	if e.test != nil {
		e.test.Engine = x
	}
}

// Kind is the engine the kind opened on this host.
func (e *Engine) Kind() kind.Engine { return e.kind }

// Host is the shared part of the engine its kind got (port.Host).
func (e *Engine) Host() kind.Host { return e.host }

// AddCloser adds a component that Close closes.
func (e *Engine) AddCloser(c port.Closer) { e.closers = append(e.closers, c) }

// Close closes the components (database connections, hub listeners).
func (e *Engine) Close() error {
	var errs []error
	for i := len(e.closers) - 1; i >= 0; i-- {
		errs = append(errs, e.closers[i].Close())
	}
	e.closers = nil
	return errors.Join(errs...)
}

func (e *Engine) closer(x any) {
	if c, ok := x.(port.Closer); ok {
		e.closers = append(e.closers, c)
	}
}

// Options are what the server sets, not engine.yaml.
type Options struct {
	Logger *slog.Logger
	// Test is test mode: Test authentication, the /_test/* API with
	// ephemeral tenants (engine/testapi); the timers role runs the timers of
	// ordinary tenants only — those of ephemeral ones fire through
	// /_test/{tenant}/clock.
	Test  bool
	Clock port.Clock
	// Models are the model interpreters by name (base, extensions): test
	// mode creates ephemeral tenants from them.
	Models map[string]reasonr.Interpreter
	// UI holds the model interfaces for the app API; nil — none.
	UI ports.UICatalog
}

// Build builds the engine from the configuration for the tenant programs.
func Build(cfg *config.Config, programs *kernel.Programs, opts Options) (*Engine, error) {
	log := opts.Logger
	if log == nil {
		log = telemetry.Discard()
	}
	e := &Engine{Programs: programs, Log: log}
	var test, demo *testapi.API
	if opts.Test {
		test = &testapi.API{Programs: programs, Models: opts.Models, Clock: testapi.NewClock(), Demo: cfg.Demo}
		e.test = test
		if opts.Clock == nil {
			opts.Clock = test.Clock
		}
	} else if len(cfg.Demo) > 0 {
		// A production server with demo tenants: the scenario clock and
		// issuing numbers for reasonrd demo load (RFC §27.5).
		demo = &testapi.API{Programs: programs, Clock: testapi.NewClock(), Demo: cfg.Demo}
	}
	var metrics port.Metrics = ports.Nop{}
	if cfg.Metrics != nil {
		switch t := config.Type(cfg.Metrics); t {
		case "prometheus":
			if err := cfg.Metrics.Only("type", "listen"); err != nil {
				return nil, err
			}
			e.Prometheus = telemetry.NewPrometheus()
			e.MetricsAddr, _ = cfg.Metrics.String("listen", "")
			metrics = e.Prometheus
		default:
			return nil, fmt.Errorf("metrics %q are not in the build (prometheus); OTLP is the adapters/otel module", t)
		}
	}
	deps := registry.Deps{Catalog: programs, Logger: log, Metrics: metrics, Delivery: cfg.Delivery, Test: opts.Test, Demo: cfg.Demo, Issuers: issuers(cfg),
		Workers: cfg.RequestWorkers, UI: opts.UI}
	switch {
	case cfg.Crypto.KMS != nil:
		var err error
		if deps.KMS, err = BuildKMS(cfg.Crypto.KMS, deps); err != nil {
			return nil, err
		}
	case opts.Test:
		deps.KMS = pii.NewMemoryKMS()
	}
	e.KMS = deps.KMS
	if signer, err := BuildSigner(cfg.IntegrationsAuth, opts.Test); err != nil {
		return nil, err
	} else if signer != nil {
		deps.Signer = signer
	}
	if e.MetricsAddr == "" {
		deps.MetricsHandler = e.Prometheus.Handler()
	}
	if test != nil {
		deps.Fakes = test.Fakes(log)
	}
	stores := map[string]any{}
	for name, n := range cfg.Stores {
		f, err := registry.Store(config.Type(n))
		if err != nil {
			return nil, fmt.Errorf("stores.%s: %w", name, err)
		}
		raw, err := f(n, deps)
		if err != nil {
			_ = e.Close()
			return nil, fmt.Errorf("stores.%s: %w", name, err)
		}
		// A row store of another repository is seen as model values (plan 0021).
		stores[name] = rows.Wrap(raw, programs, rows.Options{KMS: deps.KMS, Keys: pii.Options{Subjects: cfg.Crypto.Subjects, SubjectTTL: cfg.Crypto.TTL}})
		e.closer(stores[name])
	}
	e.Stores = stores
	main, ok := stores[cfg.State].(kind.UnitStore)
	if !ok {
		return nil, fmt.Errorf("state: store %q cannot be the main store (no UnitStore)", cfg.State)
	}
	e.Store = main
	if sh, ok := main.(pii.Shared); ok { // RFC §31.2: one set of keys for every store
		for name, st := range stores {
			if o, ok := st.(pii.Shared); ok && name != cfg.State {
				o.UseKeyring(sh.Keyring())
			}
		}
	}
	e.Timers, _ = stores[cfg.State].(kind.TimerSource)
	deps.State = main
	async, byStore, err := placeProjections(cfg, programs, stores)
	if err != nil {
		return nil, err
	}
	if err := e.buildDispatcher(cfg, programs, stores, byStore, deps, opts.Clock); err != nil {
		_ = e.Close()
		return nil, err
	}
	hf, err := registry.Hub(config.Type(cfg.Hub))
	if err != nil {
		return nil, fmt.Errorf("notifications.hub: %w", err)
	}
	link, err := hf(cfg.Hub, deps)
	if err != nil {
		return nil, fmt.Errorf("notifications.hub: %w", err)
	}
	var nhub kind.NotificationHub = memory.NewHub(false)
	if link != nil {
		h := hub.New(link, e.Log)
		e.closer(h)
		nhub = h
	}
	hub := nhub
	featureHub, _ := hub.(ports.FeatureHub)  // before the test mode wrapper
	if fh, ok := hub.(ports.ForgetHub); ok { // drop cached keys on messages of other instances
		for _, st := range stores {
			if kc, ok := rows.As[ports.KeyCache](st); ok {
				fh.OnForgotten(kc.DropKeys)
			}
		}
	}
	if test != nil {
		test.Hub = testapi.NewHub(hub)
		hub = test.Hub
		test.Timers = e.Timers
		test.Effects, _ = main.(kind.EffectSource)
		test.Store = main
		for _, st := range stores {
			if d, ok := st.(testapi.Dropper); ok {
				test.Stores = append(test.Stores, d)
			}
		}
		deps.TestAPI = test.Handler()
	}
	if demo != nil {
		demo.Timers = e.Timers
		deps.TestAPI = demo.DemoHandler()
	}
	e.closer(hub)
	e.Hub = hub
	if e.Features, err = buildFeatures(cfg, programs, stores, featureHub, log); err != nil {
		return nil, err
	}
	if test != nil {
		test.Features = e.Features
	}
	chain := auth.Chain{}
	for _, n := range cfg.Auth {
		f, err := registry.Auth(config.Type(n))
		if err != nil {
			return nil, fmt.Errorf("auth: %w", err)
		}
		scheme, a, err := f(n, deps)
		if err != nil {
			return nil, fmt.Errorf("auth %s: %w", config.Type(n), err)
		}
		chain[scheme] = a
	}
	switch {
	case opts.Test:
		chain["Test"] = auth.Test{Catalog: programs}
	case len(cfg.Demo) > 0: // demo tenants: test sign-in for reasonrd demo load (RFC §27.5)
		chain["Test"] = auth.Test{Catalog: programs, Tenants: cfg.Demo}
	}
	for _, n := range cfg.Ingress {
		f, err := registry.Listener(config.Type(n))
		if err != nil {
			return nil, fmt.Errorf("ingress: %w", err)
		}
		l, err := f(n, deps)
		if err != nil {
			return nil, fmt.Errorf("ingress %s: %w", config.Type(n), err)
		}
		e.Listeners = append(e.Listeners, l)
	}
	onRequests := func() {
		for _, f := range e.wakers {
			f()
		}
	}
	kc := kernel.Config{
		Programs: programs, Store: main, Async: async, Timers: e.Timers, Hub: hub, Clock: opts.Clock,
		Retries: cfg.Limits.Retries, ConsistencyWait: cfg.Limits.ConsistencyWait, IdempotencyTTL: cfg.Limits.IdempotencyTTL,
		Logger: log, Metrics: metrics, OnRequests: onRequests, Features: e.Features, Contexts: cfg.Contexts,
	}
	if e.Notifier != nil {
		kc.Notify = e.Notifier.Notify
	}
	// The engine kind (engine: in engine.yaml) opens on this host: the
	// executor (stateless: a kernel over the main store) and its roles.
	k, err := registry.Kind(cfg.Engine)
	if err != nil {
		_ = e.Close()
		return nil, err
	}
	h := &hostOf{e: e, cfg: cfg, opts: opts, metrics: metrics, kc: kc}
	eng, err := k.Open(context.Background(), h)
	if err != nil {
		_ = e.Close()
		return nil, err
	}
	e.kind, e.host, e.kindName = eng, h, cmp.Or(cfg.Engine, "stateless")
	e.closer(eng)
	e.Executor = eng.Executor()
	// The test and demo APIs run on a kernel the kind gives; a kind of its
	// own sets its test engine (Host.SetTestEngine).
	if k, ok := e.Executor.(kind.Kernel); ok {
		if test != nil {
			test.Kernel = k
		}
		if demo != nil {
			demo.Kernel = k
		}
	}
	e.Edge = ports.EdgeDeps{Auth: chain, Logger: log, Metrics: metrics, Tracer: ports.Nop{}}
	return e, nil
}

// buildFeatures builds the feature flag rollout on the features store and
// the notification hub; Run reads the rules.
func buildFeatures(cfg *config.Config, programs *kernel.Programs, stores map[string]any, hub ports.FeatureHub, log *slog.Logger) (*features.Resolver, error) {
	fs, ok := rows.As[port.FeatureStore](stores[cfg.Features])
	if !ok {
		for _, t := range programs.Tenants() {
			if s, err := programs.Schema(t); err == nil && len(s.Features) > 0 {
				return nil, fmt.Errorf("features: store %q does not keep feature flags", cfg.Features)
			}
		}
	}
	return features.New(fs, hub, log), nil
}

// BuildKMS creates the KMS from the crypto.kms node; reasonrd pii rotate
// builds the previous master key the same way.
func BuildKMS(n *config.Node, deps registry.Deps) (port.KMS, error) {
	f, err := registry.KMS(config.Type(n))
	if err != nil {
		return nil, fmt.Errorf("crypto.kms: %w", err)
	}
	kms, err := f(n, deps)
	if err != nil {
		return nil, fmt.Errorf("crypto.kms: %w", err)
	}
	return kms, nil
}

// placeProjections returns the stores of asynchronous projections by name
// and checks the placement: a projection with a unique index cannot be
// asynchronous; the projection name must be in the model.
func placeProjections(cfg *config.Config, programs *kernel.Programs, stores map[string]any) (map[string]kind.ProjectionStore, map[string]map[string]bool, error) {
	projections := map[string]*reasonr.TypeInfo{}
	for _, t := range programs.Tenants() {
		s, err := programs.Schema(t)
		if err != nil {
			return nil, nil, err
		}
		for i := range s.Types {
			if s.Types[i].Kind == reasonr.TypeProjection {
				projections[s.Types[i].Name] = &s.Types[i]
			}
		}
	}
	known := map[string]bool{}
	for name := range projections {
		known[name] = true
	}
	for name := range cfg.Projections {
		if !config.KnownProjection(name, known) && name != "default" {
			return nil, nil, fmt.Errorf("projections.%s: no such projection in the tenant models", name)
		}
	}
	async := map[string]kind.ProjectionStore{}
	byStore := map[string]map[string]bool{}
	for name, ti := range projections {
		store, _, _ := cfg.Projection(name)
		if store == cfg.State {
			continue
		}
		ps, ok := stores[store].(kind.ProjectionStore)
		if !ok {
			return nil, nil, fmt.Errorf("projections.%s: store %q does not keep projections", name, store)
		}
		for _, in := range ti.Indexes {
			if in.Unique {
				return nil, nil, fmt.Errorf("projections.%s: a projection with unique index %s can only be synchronous (in %q)",
					name, in.Name, cfg.State)
			}
		}
		async[name] = ps
		if byStore[store] == nil {
			byStore[store] = map[string]bool{}
		}
		byStore[store][name] = true
	}
	return async, byStore, nil
}

// buildDispatcher builds the effect stream consumers (asynchronous
// projections by store, the event store, the forget journal), the external
// request sender and the bus.
func (e *Engine) buildDispatcher(cfg *config.Config, programs *kernel.Programs, stores map[string]any, byStore map[string]map[string]bool, deps registry.Deps, clock port.Clock) error {
	var events ports.EventStore
	if cfg.Events != "" {
		sink, ok := stores[cfg.Events].(ports.EventStore)
		if !ok {
			return fmt.Errorf("events: store %q cannot be the event store", cfg.Events)
		}
		events = sink
	}
	if cfg.Crypto.ForgetLog != "" {
		if _, ok := rows.As[port.ForgetLog](stores[cfg.Crypto.ForgetLog]); !ok {
			return fmt.Errorf("crypto.forget_log: store %q does not keep the forget journal", cfg.Crypto.ForgetLog)
		}
	}
	transports := map[string]ports.Transport{}
	if queue, ok := e.Store.(kind.RequestQueue); ok {
		// Without integrations the sender works too: it drops the requests
		// of systems without a transport (C0004 warns about it).
		e.Requests = &dispatch.Sender{Queue: queue, Systems: map[string]ports.Transport{}, Catalog: programs, Clock: clock,
			Workers: cfg.RequestWorkers, Retry: dispatch.RetryPolicy(cfg.RequestRetry), Log: e.Log, Metrics: deps.Metrics}
		for name, n := range cfg.Integrations {
			f, err := registry.Integration(config.Type(n))
			if err != nil {
				return fmt.Errorf("integrations.%s: %w", name, err)
			}
			t, err := f(n, deps)
			if err != nil {
				return fmt.Errorf("integrations.%s: %w", name, err)
			}
			transports[name] = t
			if name == "default" {
				e.Requests.Default = t
			} else {
				e.Requests.Systems[name] = t
			}
		}
	} else if len(cfg.Integrations) > 0 {
		return fmt.Errorf("integrations: the main store %q does not keep external requests (port.RequestQueue)", cfg.State)
	}
	var pub, broker port.Publisher
	if cfg.Bus != nil {
		f, err := registry.Publisher(config.Type(cfg.Bus))
		if err != nil {
			return fmt.Errorf("bus: %w", err)
		}
		if pub, err = f(cfg.Bus, deps); err != nil {
			return fmt.Errorf("bus: %w", err)
		}
		e.closer(pub)
		// The adapter gets ready messages: the topic and the CloudEvents attributes.
		broker = pub
		ready := &bus.Ready{Publisher: pub}
		if tb, ok := pub.(port.TopicBroker); ok {
			ready.Template = tb.TopicTemplate()
		}
		pub = ready
		if cfg.BusCodec != "" && cfg.BusCodec != "json" {
			c, err := registry.Codec(cfg.BusCodec)
			if err != nil {
				return fmt.Errorf("bus.codec: %w", err)
			}
			pub = &bus.Recoder{Publisher: pub, Catalog: programs, Codec: c}
		}
		if events == nil { // C0013
			return fmt.Errorf("bus: the bus reads events from the event store: set events")
		}
		if cfg.BusNotifications {
			e.Notifier = &dispatch.Notifier{Publisher: pub, Catalog: programs, Log: e.Log, Metrics: deps.Metrics}
		}
	}
	e.Topics = topicsOf(cfg, programs, broker, transports)
	if pub == nil {
		return nil
	}
	feed, ok := rows.As[port.EventFeed](events)
	if !ok {
		return fmt.Errorf("bus: event store %q cannot give events to the bus (port.EventFeed)", cfg.Events)
	}
	e.Bus = &dispatch.Bus{Feed: feed, Publisher: pub, Coord: e.coordinator(), Log: e.Log, Metrics: deps.Metrics}
	e.busWake = e.Bus.Wake
	return nil
}

// Run runs the process roles until ctx is cancelled; the first role error
// stops the others.
func (e *Engine) Run(ctx context.Context, roles []string) error {
	known := e.Roles()
	for _, r := range roles {
		if !slices.Contains(known, r) {
			return fmt.Errorf("unknown role %q (%s)", r, strings.Join(known, ", "))
		}
	}
	if e.Features != nil {
		if err := e.Features.Load(ctx); err != nil {
			return fmt.Errorf("features: %w", err)
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, len(e.Listeners)+5)
	run := 0
	start := func(name string, f func(context.Context) error) {
		run++
		go func() {
			err := f(ctx)
			if err != nil {
				err = fmt.Errorf("%s: %w", name, err)
			}
			errs <- err
		}()
	}
	if slices.Contains(roles, "api") {
		x := e.Executor
		if x == nil && len(e.Listeners) > 0 {
			return fmt.Errorf("the api role: the engine kind %q gives no executor", e.kindName)
		}
		for _, l := range e.Listeners {
			start("api", func(ctx context.Context) error { return l.Serve(ctx, x, e.Edge) })
		}
		if e.MetricsAddr != "" && e.Prometheus != nil {
			start("metrics", e.serveMetrics)
		}
	}
	if e.kind != nil {
		for _, kr := range e.kind.Roles() {
			if kr.Name == "" || slices.Contains(roles, kr.Name) {
				start(cmp.Or(kr.Name, "engine"), kr.Run)
			}
		}
	}
	if slices.Contains(roles, "dispatch") && e.Bus != nil {
		start("bus", e.Bus.Run)
	}
	if e.Notifier != nil {
		start("notifications", e.Notifier.Run)
	}
	if e.Features != nil && e.Features.Store != nil {
		start("features", e.Features.Run)
	}
	e.Log.Info("engine started", "roles", roles, "tenants", len(e.Programs.Tenants()))
	var first error
	for range run {
		err := <-errs
		if err != nil && first == nil {
			first = err
			cancel()
		}
	}
	return first
}

// coordinator is the coordinator of the main store's effect stream and the
// bus: the store's own, or one in memory for a store of one process.
func (e *Engine) coordinator() port.Coordinator {
	if coord, ok := rows.As[port.Coordinator](e.Store); ok {
		return coord
	}
	e.coordOnce.Do(func() { e.coord = memory.NewCoordinator() })
	return e.coord
}

// Roles are the roles of this process: the host's and the kind's.
func (e *Engine) Roles() []string {
	out := slices.Clone(Roles)
	if e.kind != nil {
		for _, kr := range e.kind.Roles() {
			if kr.Name != "" && !slices.Contains(out, kr.Name) {
				out = append(out, kr.Name)
			}
		}
	}
	return out
}

func (e *Engine) serveMetrics(ctx context.Context) error {
	srv := &http.Server{Addr: e.MetricsAddr, Handler: e.Prometheus.Handler(), ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// issuers are the token issuers from auth (issuer of jwt).
func issuers(cfg *config.Config) []string {
	var out []string
	for _, n := range cfg.Auth {
		if v, _ := n.String("issuer", ""); v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}
