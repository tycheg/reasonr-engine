package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/bus"
	"github.com/tycheg/reasonr-engine/internal/config"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/internal/registry"
	"github.com/tycheg/reasonr-engine/internal/testapi"
	"github.com/tycheg/reasonr-engine/port"
)

// Topic is a broker topic the engine needs (RFC §31.1).
type Topic struct {
	Name string
	// Component is who writes to the topic: bus or integrations.<name>.
	Component string
	// Settings are the component's create_topics settings; nil — not set.
	Settings *port.TopicSettings
	broker   port.TopicBroker
}

// topicsOf derives the topics of the bus pub and the request transports (by
// name in integrations) from the tenant models: for the bus — the events of
// every type with events and, with bus.notifications, the notifications of
// every type with notifications; for a transport — the requests of the
// systems it serves (its own integrations entry or default). Components
// without topics (port.TopicBroker) are skipped.
func topicsOf(cfg *config.Config, programs *kernel.Programs, pub any, transports map[string]ports.Transport) []Topic {
	seen := map[string]bool{}
	var out []Topic
	add := func(b port.TopicBroker, component, name string) {
		if !seen[name] {
			seen[name] = true
			out = append(out, Topic{Name: name, Component: component, Settings: b.TopicSettings(), broker: b})
		}
	}
	busBroker, _ := pub.(port.TopicBroker)
	for _, tenant := range programs.Tenants() {
		s, err := programs.Schema(tenant)
		if err != nil {
			continue
		}
		for i := range s.Types {
			ti := &s.Types[i]
			for _, mi := range ti.Methods {
				switch {
				case busBroker != nil && mi.Kind == reasonr.MethodEvent:
					add(busBroker, "bus", bus.Topic(busBroker.TopicTemplate(), port.MessageEvent, tenant, ti.Name))
				case busBroker != nil && mi.Kind == reasonr.MethodNotification && cfg.BusNotifications:
					add(busBroker, "bus", bus.Topic(busBroker.TopicTemplate(), port.MessageNotification, tenant, ti.Name))
				case mi.Kind == reasonr.MethodRequest:
					name := ti.Short()
					if transports[name] == nil {
						name = "default"
					}
					if b, ok := ports.TopicBroker(transports[name]); ok {
						add(b, "integrations."+name, bus.RequestTopic(b.TopicTemplate(), tenant, ti.Short()))
					}
				}
			}
		}
	}
	slices.SortFunc(out, func(a, b Topic) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Topics returns the broker topics the bus and the request transports of
// the configuration cfg need for the tenant models, without connecting to
// stores or the broker (reasonrd topics: a list for Terraform, GitOps).
func Topics(cfg *config.Config, programs *kernel.Programs, opts Options) ([]Topic, error) {
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	deps := registry.Deps{Catalog: programs, Logger: log, Metrics: ports.Nop{}, Test: opts.Test}
	if opts.Test {
		deps.Fakes = (&testapi.API{}).Fakes(log)
	}
	var closers []any
	defer func() {
		for _, c := range closers {
			if c, ok := c.(port.Closer); ok {
				_ = c.Close()
			}
		}
	}()
	var pub any
	if cfg.Bus != nil {
		f, err := registry.Publisher(config.Type(cfg.Bus))
		if err != nil {
			return nil, fmt.Errorf("bus: %w", err)
		}
		if pub, err = f(cfg.Bus, deps); err != nil {
			return nil, fmt.Errorf("bus: %w", err)
		}
		closers = append(closers, pub)
	}
	transports := map[string]ports.Transport{}
	for name, n := range cfg.Integrations {
		f, err := registry.Integration(config.Type(n))
		if err != nil {
			return nil, fmt.Errorf("integrations.%s: %w", name, err)
		}
		t, err := f(n, deps)
		if err != nil {
			return nil, fmt.Errorf("integrations.%s: %w", name, err)
		}
		transports[name] = t
		closers = append(closers, t)
	}
	return topicsOf(cfg, programs, pub, transports), nil
}

// topicsTimeout is how long to wait for the broker when checking and creating topics.
const topicsTimeout = 30 * time.Second

// EnsureTopics checks that the topics e.Topics exist in the brokers.
// Missing ones are created if create (reasonrd migrate) and the component
// has create_topics, and always in test mode (without create_topics — with
// one partition); otherwise — an error with the list. Existing topics do
// not change: another number of partitions than in create_topics is a
// warning. An unavailable broker is a warning: the engine API does not
// depend on the broker, and the messages wait for it.
func (e *Engine) EnsureTopics(ctx context.Context, create bool) error {
	var brokers []port.TopicBroker
	byBroker := map[port.TopicBroker][]Topic{}
	for _, t := range e.Topics {
		if byBroker[t.broker] == nil {
			brokers = append(brokers, t.broker)
		}
		byBroker[t.broker] = append(byBroker[t.broker], t)
	}
	ctx, cancel := context.WithTimeout(ctx, topicsTimeout)
	defer cancel()
	var absent []string
	var errs []error
	for _, b := range brokers {
		ts := byBroker[b]
		component := ts[0].Component
		names := make([]string, len(ts))
		for i, t := range ts {
			names[i] = t.Name
		}
		have, err := b.DescribeTopics(ctx, names)
		if err != nil {
			e.Log.Warn("topics not checked: the broker is unavailable", "component", component, "error", err)
			continue
		}
		var missing []string
		for _, t := range ts {
			p, ok := have[t.Name]
			switch {
			case !ok:
				missing = append(missing, t.Name)
			case t.Settings != nil && t.Settings.Partitions > 0 && p != t.Settings.Partitions:
				e.Log.Warn("the topic has another number of partitions than in create_topics: the engine does not change it", "topic", t.Name,
					"partitions", p, "create_topics", t.Settings.Partitions)
			}
		}
		if len(missing) == 0 {
			continue
		}
		s := b.TopicSettings()
		if s == nil && e.test != nil {
			s = &port.TopicSettings{Partitions: 1}
		}
		if s == nil || (!create && e.test == nil) {
			absent = append(absent, missing...)
			continue
		}
		if err := b.CreateTopics(ctx, missing, *s); err != nil {
			errs = append(errs, fmt.Errorf("%s: topics not created: %w", component, err))
			continue
		}
		e.Log.Info("topics created", "component", component, "topics", missing)
	}
	if len(absent) > 0 {
		slices.Sort(absent)
		errs = append(errs, fmt.Errorf("no topics %s — create them (the list is reasonrd topics) or set create_topics and run reasonrd migrate",
			strings.Join(absent, ", ")))
	}
	return errors.Join(errs...)
}
