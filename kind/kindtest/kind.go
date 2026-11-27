package kindtest

import (
	"context"
	"time"

	"github.com/tycheg/reasonr-engine/internal/registry"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// testKind is a test double of an engine kind for the engine's own tests,
// which cannot link the stateless module (reasonr-stateless): a kernel over
// the main store, its fired timers, the effect stream of the main store.
// Production engines are the stateless kind and the kinds of other modules.
type testKind struct{}

var _ kind.Kind = testKind{}

func (testKind) Open(_ context.Context, h kind.Host) (kind.Engine, error) {
	k, err := h.NewKernel(h.KernelConfig())
	if err != nil {
		return nil, err
	}
	e := &engine{h: h, k: k}
	src, ok := h.State().(kind.EffectSource)
	if !ok {
		return e, nil
	}
	var consumers []kind.Consumer
	if cfg := h.Config(); cfg.Contexts != nil {
		consumers = append(consumers, h.ACL(cfg.Contexts, h.Programs(), k.Deliver))
	}
	queue, _ := h.State().(kind.RequestQueue)
	effects, err := h.Effects(kind.EffectsConfig{Source: src, Catalog: h.Programs(), Consumers: consumers, Requests: queue, Logger: h.Log()})
	if err != nil {
		return nil, err
	}
	if effects.Wake != nil {
		h.OnRequests(effects.Wake)
	}
	e.effects = effects
	return e, nil
}

// TestRoles: the tests run the timers and the effect stream next to the engine.
func (testKind) TestRoles() []string { return []string{"timers", "dispatch"} }

type engine struct {
	h       kind.Host
	k       kind.Kernel
	effects *kind.Effects
}

func (e *engine) Executor() kind.Executor { return e.k }
func (e *engine) Close() error            { return nil }

func (e *engine) Roles() []kind.Role {
	roles := []kind.Role{{Name: "timers", Run: e.timers}}
	if e.effects != nil {
		roles = append(roles, kind.Role{Name: "dispatch", Run: e.effects.Run(e.h.Coordinator())})
	}
	return roles
}

func (e *engine) timers(ctx context.Context) error {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if e.h.Timers() == nil {
				continue
			}
			tenants, all := e.h.TimerTenants()
			if all {
				tenants = []port.Tenant{""}
			}
			for _, t := range tenants {
				ts, _ := e.h.Timers().Lease(ctx, port.LeaseRequest{Tenant: t, Now: e.k.Clock().Now(t), Limit: 100, TTL: kind.TimerLease})
				for i := range ts {
					if res, err := e.k.Fire(ctx, &ts[i]); err == nil && res != nil {
						res.Release()
					}
				}
			}
		}
	}
}

// UseKind registers a test double of the stateless engine kind (a kernel
// over the main store, its timers, its effect stream) unless the test links
// the real one (reasonr-stateless).
func UseKind() {
	if _, err := registry.Kind("stateless"); err != nil {
		kind.RegisterKind("stateless", testKind{})
	}
}
