package memory

import (
	"context"
	"sort"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/pii"
	"github.com/tycheg/reasonr-engine/internal/storekit"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// outboxEntry is a unit of work in the outbox: values without personal ones
// (none) in its own arena and the ciphertexts of personal values.
type outboxEntry struct {
	unit     kind.CommittedUnit
	events   [][]storekit.Sealed // by the events of all entities in order
	ops      [][]storekit.Sealed // by operation; Field is the number in Set
	requests [][]storekit.Sealed
}

// sealed reports whether the entry has personal values.
func (e *outboxEntry) sealed() bool {
	for _, group := range [][][]storekit.Sealed{e.events, e.ops, e.requests} {
		for _, ss := range group {
			if len(ss) > 0 {
				return true
			}
		}
	}
	return false
}

// outboxEntry prepares the outbox entry of a unit of work: entity events,
// operations on asynchronous projections, external requests and forget.
// The position is set at commit.
func (s *Store) outboxEntry(ctx context.Context, seal *pii.Sealer, sp *space, tenant port.Tenant, u *kind.Unit, rows []*pending) (*outboxEntry, error) {
	a, src := reasonr.NewArena(), u.Arena
	e := &outboxEntry{unit: kind.CommittedUnit{Tenant: tenant, At: u.Now, Arena: a, Contexts: u.Contexts}}
	for _, p := range rows {
		if len(p.events) == 0 {
			continue
		}
		ee := kind.EntityEvents{Type: p.t, ID: a.Copy(src, p.id), Version: p.version + 1}
		for i, ev := range p.events {
			ev.Args = a.Copy(src, p.evStripped[i]).Record()
			ee.Events = append(ee.Events, ev)
			e.events = append(e.events, p.evSealed[i])
		}
		e.unit.Entities = append(e.unit.Entities, ee)
	}
	ops := make([]reasonr.ProjectionOp, len(u.Async))
	for i := range u.Async {
		op, sealed, err := sp.crypto.SealOp(ctx, seal, src, u.Async[i])
		if err != nil {
			return nil, err
		}
		ops[i] = op
		e.ops = append(e.ops, sealed)
	}
	e.unit.Projections = copyOps(a, src, ops)
	for _, rq := range u.Work.Requests {
		ti := sp.schema.Type(rq.System)
		mi := &ti.Methods[rq.Request]
		args, sealed, err := sp.crypto.Seal(ctx, seal, src, ti.Name+"."+mi.Name, mi.Params, rq.Args.Value(), sp.crypto.ArgSubjects(src, rq.Subjects))
		if err != nil {
			return nil, err
		}
		rq.Args, rq.Key, rq.Subjects = a.Copy(src, args).Record(), a.Copy(src, rq.Key), nil
		e.unit.Requests = append(e.unit.Requests, rq)
		e.requests = append(e.requests, sealed)
	}
	for _, subj := range u.Work.Forget {
		e.unit.Forget = append(e.unit.Forget, reasonr.Subject{Type: subj.Type, ID: a.Copy(src, subj.ID)})
	}
	e.unit.Calls = copyCalls(a, src, u.Work.Calls)
	return e, nil
}

// copyCalls copies the fake calls into the arena a. They have no personal
// values: a fake keeps test data (E1209).
func copyCalls(a, src *reasonr.Arena, calls []reasonr.FakeCall) []reasonr.FakeCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]reasonr.FakeCall, len(calls))
	for i, c := range calls {
		c.ID, c.Args = a.Copy(src, c.ID), a.Copy(src, c.Args.Value()).Record()
		out[i] = c
	}
	return out
}

// openEntry returns the unit of work of an outbox entry with decrypted
// personal values — a copy in a new arena; an entry without them is
// returned as is. Called under the lock.
func (s *Store) openEntry(ctx context.Context, e *outboxEntry) (kind.CommittedUnit, error) {
	sp := s.spaces[e.unit.Tenant]
	if !e.sealed() || sp == nil {
		return e.unit, nil // the tenant is deleted: personal values stay none
	}
	seal := s.ring.Sealer(e.unit.Tenant)
	src, a := e.unit.Arena, reasonr.NewArena()
	cu := e.unit
	cu.Arena = a
	cu.Entities = nil
	n := 0
	for _, ee := range e.unit.Entities {
		ti := sp.schema.Type(ee.Type)
		ee.ID = a.Copy(src, ee.ID)
		evs := make([]reasonr.Event, len(ee.Events))
		for i, ev := range ee.Events {
			mi := &ti.Methods[ev.Name]
			args := a.Copy(src, ev.Args.Value())
			if err := sp.crypto.Open(ctx, seal, a, ti.Name+"."+mi.Name, mi.Params, args, e.events[n]); err != nil {
				return kind.CommittedUnit{}, err
			}
			ev.Args = args.Record()
			subjects, err := sp.crypto.ArgSubjectsOf(a, e.events[n])
			if err != nil {
				return kind.CommittedUnit{}, err
			}
			ev.Subjects = subjects
			evs[i] = ev
			n++
		}
		ee.Events = evs
		cu.Entities = append(cu.Entities, ee)
	}
	cu.Projections = copyOps(a, src, e.unit.Projections)
	for i, op := range cu.Projections {
		if err := sp.crypto.OpenOp(ctx, seal, a, op, e.ops[i]); err != nil {
			return kind.CommittedUnit{}, err
		}
	}
	cu.Requests = make([]reasonr.ExternalRequest, len(e.unit.Requests))
	for i, rq := range e.unit.Requests {
		ti := sp.schema.Type(rq.System)
		mi := &ti.Methods[rq.Request]
		args := a.Copy(src, rq.Args.Value())
		if err := sp.crypto.Open(ctx, seal, a, ti.Name+"."+mi.Name, mi.Params, args, e.requests[i]); err != nil {
			return kind.CommittedUnit{}, err
		}
		subjects, err := sp.crypto.ArgSubjectsOf(a, e.requests[i])
		if err != nil {
			return kind.CommittedUnit{}, err
		}
		rq.Args, rq.Key, rq.Subjects = args.Record(), a.Copy(src, rq.Key), subjects
		cu.Requests[i] = rq
	}
	cu.Forget = make([]reasonr.Subject, len(e.unit.Forget))
	for i, subj := range e.unit.Forget {
		cu.Forget[i] = reasonr.Subject{Type: subj.Type, ID: a.Copy(src, subj.ID)}
	}
	cu.Calls = copyCalls(a, src, e.unit.Calls)
	cu.Keys = seal.Opened()
	return cu, nil
}

// Trim deletes from the outbox the units of work with a position no
// greater than upto: every consumer has processed them (an engine kind
// that keeps its state in memory trims by the smallest cursor).
func (s *Store) Trim(upto port.Position) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := sort.Search(len(s.outbox), func(i int) bool { return s.outbox[i].unit.Position > upto })
	if i > 0 {
		s.outbox = append(s.outbox[:0:0], s.outbox[i:]...)
	}
}
