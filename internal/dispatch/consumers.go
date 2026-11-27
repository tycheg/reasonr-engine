package dispatch

import (
	"context"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/internal/storekit"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Projections are the asynchronous projections of one store: operations on
// its projections are applied by position; a unit without them only
// records the position, so that Applied catches up with the stream (after).
type Projections struct {
	ConsumerName string
	Store        kind.ProjectionStore
	Catalog      kind.Catalog
	Names        map[string]bool // the names of this store's projections
}

func (p *Projections) Name() string { return p.ConsumerName }

func (p *Projections) Deliver(ctx context.Context, u *kind.CommittedUnit, _ int, _ func(int, string) error) error {
	s, err := p.Catalog.Schema(u.Tenant)
	if err != nil {
		return err
	}
	var ops []reasonr.ProjectionOp
	for _, op := range u.Projections {
		if ti := s.Type(op.Projection); ti != nil && p.Names[ti.Name] {
			ops = append(ops, op)
		}
	}
	var forget kind.Forget
	if len(u.Forget) > 0 {
		forget.Subjects = u.Forget
		for name := range p.Names {
			if ti := s.Lookup(name); ti != nil {
				forget.Types = append(forget.Types, ti.Ref)
			}
		}
	}
	return p.Store.Apply(ctx, u.Tenant, u.Position, u.Arena, ops, u.Keys, forget)
}

// Events is the event store.
type Events struct {
	Sink ports.EventStore
	// OnAppend is called after writing a unit's events: it wakes this
	// process's bus (Bus.Wake); nil — not called.
	OnAppend func()
}

func (e *Events) Name() string { return EventsConsumer }

func (e *Events) Deliver(ctx context.Context, u *kind.CommittedUnit, _ int, _ func(int, string) error) error {
	if len(u.Entities) == 0 {
		return nil
	}
	if err := e.Sink.Append(ctx, []kind.CommittedUnit{*u}); err != nil {
		return err
	}
	if e.OnAppend != nil {
		e.OnAppend()
	}
	return nil
}

// DeliverBatch writes the events of a batch in one operation.
func (e *Events) DeliverBatch(ctx context.Context, units []kind.CommittedUnit) error {
	if err := e.Sink.Append(ctx, units); err != nil {
		return err
	}
	if e.OnAppend != nil {
		e.OnAppend()
	}
	return nil
}

// Forgets is the forget journal (RFC §20.3): the subjects forgotten by
// units of work, in the crypto.forget_log store.
type Forgets struct {
	Log     port.ForgetLog
	Catalog kind.Catalog
}

func (f *Forgets) Name() string { return "forget_log" }

func (f *Forgets) Deliver(ctx context.Context, u *kind.CommittedUnit, _ int, _ func(int, string) error) error {
	if len(u.Forget) == 0 {
		return nil
	}
	s, err := f.Catalog.Schema(u.Tenant)
	if err != nil {
		return err
	}
	refs, err := storekit.Refs(s, u.Arena, u.Forget)
	if err != nil {
		return err
	}
	entries := make([]port.Forgotten, len(refs))
	for i, ref := range refs {
		entries[i] = port.Forgotten{Tenant: u.Tenant, Position: u.Position, At: u.At, Subject: ref}
	}
	return f.Log.Record(ctx, entries)
}
