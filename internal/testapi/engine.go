package testapi

import (
	"context"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Engine is the engine under the test API: the kernel of the stateless
// engine (the default, from the API fields) or the test engine of a kind
// that runs elsewhere, whose tenants, clocks and timers live there.
type Engine = kind.TestEngine

// engine returns the API engine: the given one or the kernel from the fields.
func (api *API) engine() Engine {
	if api.Engine != nil {
		return api.Engine
	}
	return kernelEngine{api}
}

// kernelEngine is the test API on the kernel of the stateless engine.
type kernelEngine struct{ api *API }

func (k kernelEngine) Command(ctx context.Context, req *kind.CommandRequest) (*kind.CommandResult, error) {
	return k.api.Kernel.Command(ctx, req)
}

func (k kernelEngine) NewID(ctx context.Context, tenant port.Tenant, t reasonr.TypeRef) (reasonr.Value, error) {
	return k.api.Kernel.NewID(ctx, tenant, t)
}

func (k kernelEngine) Load(ctx context.Context, tenant port.Tenant, a *reasonr.Arena, t reasonr.TypeRef, id reasonr.Value) (reasonr.Record, uint64, bool, error) {
	return k.api.Store.Load(ctx, tenant, a, t, id)
}

func (k kernelEngine) Read(ctx context.Context, from port.Position, limit int) ([]kind.CommittedUnit, error) {
	return k.api.Effects.Read(ctx, from, limit)
}

func (k kernelEngine) CreateTenant(_ context.Context, t port.Tenant, model string, now time.Time) error {
	k.api.Clock.set(t, now)
	k.api.Programs.Set(t, k.api.Models[model])
	return nil
}

func (k kernelEngine) DropTenant(ctx context.Context, t port.Tenant) error {
	k.api.Programs.Remove(t)
	if k.api.Features != nil {
		k.api.Features.DropTest(t)
	}
	k.api.Clock.drop(t)
	return nil
}

func (k kernelEngine) SetTestFeature(_ context.Context, t port.Tenant, name string) error {
	k.api.Features.SetTest(t, name)
	return nil
}

func (k kernelEngine) Advance(ctx context.Context, t port.Tenant, d, offset time.Duration, scenario bool, each func(*kind.CommandResult) error) error {
	api := k.api
	if d < 0 { // back (time offset -d): timers do not fire
		if !scenario {
			api.Clock.set(t, api.Clock.Now(t).Add(d))
		}
		return nil
	}
	target := api.Clock.Now(t).Add(offset + d)
	for {
		ts, err := api.Timers.Lease(ctx, port.LeaseRequest{Tenant: t, Now: target, Limit: 1, TTL: kernel.TimerLease, Scenario: scenario})
		if err != nil {
			return err
		}
		if len(ts) == 0 {
			break
		}
		if !scenario && ts[0].At.After(api.Clock.Now(t)) {
			api.Clock.set(t, ts[0].At)
		}
		res, err := api.Kernel.Fire(ctx, &ts[0])
		if err != nil {
			return &timerError{err}
		}
		if res == nil {
			continue
		}
		err = each(res)
		res.Release()
		if err != nil {
			return err
		}
	}
	if !scenario {
		api.Clock.set(t, target)
	}
	return nil
}

// timerError is a fault of a timer command (response 500 "timer command: …").
type timerError struct{ err error }

func (e *timerError) Error() string { return "timer command: " + e.err.Error() }
func (e *timerError) Unwrap() error { return e.err }
