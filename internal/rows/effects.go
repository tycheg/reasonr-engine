package rows

import (
	"context"
	"errors"
	"fmt"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/internal/storekit"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

var (
	_ kind.TimerSource     = (*Store)(nil)
	_ kind.EffectSource    = (*Store)(nil)
	_ kind.ProjectionStore = (*Store)(nil)
	_ ports.EventStore     = (*Store)(nil)
	_ kind.RequestQueue    = (*Store)(nil)
	_ kind.StateTransfer   = (*Store)(nil)
	_ kind.EntitySink      = (*Store)(nil)
	_ ports.SchemaStore    = (*Store)(nil)
)

// errNoPort: the row store has no port for the operation.
func errNoPort(name string) error { return fmt.Errorf("rows: the store has no %s", name) }

// Lease selects fired timers and leases them (port.TimerSource).
func (s *Store) Lease(ctx context.Context, req port.LeaseRequest) ([]kind.LeasedTimer, error) {
	ts, ok := s.db.(port.TimerStore)
	if !ok {
		return nil, errNoPort("timers")
	}
	recs, err := ts.Lease(ctx, req)
	if err != nil {
		return nil, err
	}
	out := make([]kind.LeasedTimer, 0, len(recs))
	for i := range recs {
		lt, err := s.timer(ctx, recs[i].Tenant, &recs[i].TimerRecord, reasonr.NewArena())
		if err != nil {
			return nil, err
		}
		lt.Attempts = recs[i].Attempts
		out = append(out, lt)
	}
	return out, nil
}

// timer reads a timer by the tenant's schema and decrypts its personal
// arguments: for forgotten subjects they are none.
func (s *Store) timer(ctx context.Context, tenant port.Tenant, r *port.TimerRecord, a *reasonr.Arena) (kind.LeasedTimer, error) {
	m, err := s.model(tenant)
	if err != nil {
		return kind.LeasedTimer{}, err
	}
	ti := m.s.Lookup(r.Entity)
	if ti == nil {
		return kind.LeasedTimer{}, fmt.Errorf("rows: a timer for an unknown type %s", r.Entity)
	}
	mi := ti.Method(r.Command)
	if mi == nil {
		return kind.LeasedTimer{}, fmt.Errorf("rows: a timer for an unknown command %s.%s", r.Entity, r.Command)
	}
	eid, err := m.json.Decode(a, storekit.IDType(ti.Ref), r.EntityID)
	if err != nil {
		return kind.LeasedTimer{}, err
	}
	rec, err := m.json.DecodeFields(a, 0, mi.Params, r.Args)
	if err != nil {
		return kind.LeasedTimer{}, err
	}
	if pc := m.payloadCrypto(s.ring.Sealer(tenant)); pc != nil && r.Sealed != nil {
		if err := pc.OpenArgs(ctx, a, ti.Name+"."+mi.Name, mi.Params, rec.Value(), r.Sealed); err != nil {
			return kind.LeasedTimer{}, err
		}
	}
	// At is the moment by the scenario clock: the real one plus the offset.
	return kind.LeasedTimer{Tenant: tenant, ID: reasonr.TimerID(r.ID), At: r.At.Add(r.Offset), Offset: r.Offset, Entity: ti.Ref,
		EntityID: eid, Command: mi.Ref, Args: rec, Arena: a}, nil
}

// Retry releases a timer's lease after a fault (port.TimerSource).
func (s *Store) Retry(ctx context.Context, t *kind.LeasedTimer, at time.Time) error {
	ts, ok := s.db.(port.TimerStore)
	if !ok {
		return errNoPort("timers")
	}
	return ts.Retry(ctx, t.Tenant, t.ID.UUID(), at)
}

// Dead makes a timer dead (port.TimerSource).
func (s *Store) Dead(ctx context.Context, t *kind.LeasedTimer, reason string) error {
	ts, ok := s.db.(port.TimerStore)
	if !ok {
		return errNoPort("timers")
	}
	return ts.Dead(ctx, t.Tenant, t.ID.UUID(), reason)
}

// Read returns the units of work after the position from (port.EffectSource).
func (s *Store) Read(ctx context.Context, from port.Position, limit int) ([]kind.CommittedUnit, error) {
	ob, ok := s.db.(port.Outbox)
	if !ok {
		return nil, errNoPort("outbox")
	}
	recs, err := ob.Read(ctx, from, limit)
	if err != nil {
		return nil, err
	}
	out := make([]kind.CommittedUnit, 0, len(recs))
	for _, r := range recs {
		cu := kind.CommittedUnit{Tenant: r.Tenant, Position: r.Position, Arena: reasonr.NewArena()}
		m, err := s.model(cu.Tenant)
		switch {
		case errors.Is(err, port.ErrUnknownTenant):
			// The tenant is deleted (an ephemeral test tenant): the position
			// goes on, there are no effects.
		case err != nil:
			return nil, err
		default:
			if err := storekit.DecodePayload(ctx, m.json, m.payloadCrypto(s.ring.Sealer(cu.Tenant)), r.Payload, &cu); err != nil {
				return nil, fmt.Errorf("rows: outbox %d: %w", r.Position, err)
			}
		}
		out = append(out, cu)
	}
	return out, nil
}

// Wait waits for a unit of work after the position (port.EffectSource).
func (s *Store) Wait(ctx context.Context, after port.Position) error {
	ob, ok := s.db.(port.Outbox)
	if !ok {
		return errNoPort("outbox")
	}
	return ob.Wait(ctx, after)
}

// Apply applies the operations on asynchronous projections of the unit at
// position pos (port.ProjectionStore). Personal values are encrypted with
// the key versions keys (pii.Keyring.Resealer): it creates no keys.
func (s *Store) Apply(ctx context.Context, tenant port.Tenant, pos port.Position, a *reasonr.Arena, ops []reasonr.ProjectionOp,
	keys []port.KeyVersion, forget kind.Forget) error {
	ap, ok := s.db.(port.AsyncProjections)
	if !ok {
		return errNoPort("asynchronous projections")
	}
	m, err := s.model(tenant)
	if err != nil {
		return err
	}
	// The unit's arena is shared with the other consumers of the effect
	// stream, which read it at the same time, and building and sealing a
	// row writes to its arena: the delivery works on copies in its own.
	a, ops, forget.Subjects = storekit.Own(a, ops, forget.Subjects)
	rec := &port.UnitRecord{}
	if err := m.projectionRows(ctx, s.ring.Resealer(tenant, keys), a, ops, rec, nil); err != nil {
		return err
	}
	var refs []port.SubjectRef
	var tables []*port.Table
	if len(forget.Subjects) > 0 {
		if refs, err = storekit.Refs(m.s, a, forget.Subjects); err != nil {
			return err
		}
		tables = m.personalTables(forget.Types)
	}
	return ap.Apply(ctx, tenant, pos, rec.Writes, rec.Deletes, refs, tables, s.forgetRewriter(refs))
}

// Applied returns the last applied position (port.ProjectionStore).
func (s *Store) Applied(ctx context.Context, tenant port.Tenant) (port.Position, error) {
	ap, ok := s.db.(port.AsyncProjections)
	if !ok {
		return 0, errNoPort("asynchronous projections")
	}
	return ap.Applied(ctx, tenant)
}

// Append writes the events of units of work (ports.EventStore): personal
// arguments are encrypted with the key versions the outbox was decrypted
// with (CommittedUnit.Keys), and the log is not decrypted when read.
func (s *Store) Append(ctx context.Context, units []kind.CommittedUnit) error {
	el, ok := s.db.(port.EventLog)
	if !ok {
		return errNoPort("event log")
	}
	var events []port.EventRecord
	for i := range units {
		u := &units[i]
		m, err := s.model(u.Tenant)
		if err != nil {
			return err
		}
		seal := s.ring.Resealer(u.Tenant, u.Keys)
		for ei, ee := range u.Entities {
			ti := m.s.Type(ee.Type)
			id, err := m.json.Append(nil, u.Arena, storekit.IDType(ee.Type), ee.ID)
			if err != nil {
				return err
			}
			for _, ev := range ee.Events {
				mi := &ti.Methods[ev.Name]
				owner := ti.Name + "." + mi.Name
				stripped, sealed, err := m.crypto.Seal(ctx, seal, u.Arena, owner, mi.Params, ev.Args.Value(), m.crypto.ArgSubjects(u.Arena, ev.Subjects))
				if err != nil {
					return err
				}
				args, err := m.json.AppendFields(nil, u.Arena, mi.Params, stripped.Record())
				if err != nil {
					return err
				}
				sv, err := storekit.EncodeSealed(mi.Params, sealed)
				if err != nil {
					return err
				}
				events = append(events, port.EventRecord{Tenant: u.Tenant, Position: u.Position, Entity: ei, Seq: int(ev.Seq),
					Type: ti.Name, ID: id, Version: ee.Version, Name: mi.Name, Args: args, Sealed: sv, At: u.At})
			}
		}
	}
	if len(events) == 0 {
		return nil
	}
	return el.Append(ctx, events)
}

// ReadEvents passes the event store's feed to the bus (port.EventFeed).
func (s *Store) ReadEvents(ctx context.Context, after, upto port.Position, limit int) ([]port.EventRecord, error) {
	f, ok := s.db.(port.EventFeed)
	if !ok {
		return nil, errNoPort("event feed")
	}
	return f.ReadEvents(ctx, after, upto, limit)
}

// LeaseRequests leases ready external requests (port.RequestQueue).
func (s *Store) LeaseRequests(ctx context.Context, now time.Time, limit int, ttl time.Duration) ([]kind.LeasedRequest, error) {
	rq, ok := s.db.(port.Requests)
	if !ok {
		return nil, errNoPort("request queue")
	}
	recs, err := rq.LeaseRequests(ctx, now, limit, ttl)
	if err != nil {
		return nil, err
	}
	out := make([]kind.LeasedRequest, 0, len(recs))
	for _, r := range recs {
		lr, err := s.request(ctx, r.Tenant, &r.Request)
		if err != nil {
			return nil, err
		}
		lr.Position, lr.At, lr.Attempts, lr.Deadline, lr.TimerGone = r.Position, r.At, r.Attempts, r.Deadline, r.TimerGone
		out = append(out, lr)
	}
	return out, nil
}

// request reads a request by the tenant's schema and decrypts its personal
// arguments: for forgotten subjects they are none.
func (s *Store) request(ctx context.Context, tenant port.Tenant, r *port.RequestRecord) (kind.LeasedRequest, error) {
	m, err := s.model(tenant)
	if err != nil {
		return kind.LeasedRequest{}, err
	}
	ti := m.s.Lookup(r.System)
	if ti == nil || ti.Method(r.Request) == nil {
		return kind.LeasedRequest{}, fmt.Errorf("rows: a request to an unknown method %s.%s", r.System, r.Request)
	}
	mi := ti.Method(r.Request)
	a := reasonr.NewArena()
	rec, err := m.json.DecodeFields(a, 0, mi.Params, r.Args)
	if err != nil {
		return kind.LeasedRequest{}, err
	}
	if pc := m.payloadCrypto(s.ring.Sealer(tenant)); pc != nil && r.Sealed != nil {
		if err := pc.OpenArgs(ctx, a, ti.Name+"."+mi.Name, mi.Params, rec.Value(), r.Sealed); err != nil {
			return kind.LeasedRequest{}, err
		}
	}
	return kind.LeasedRequest{LeasedRequest: port.LeasedRequest{Tenant: tenant}, Arena: a,
		Request: reasonr.ExternalRequest{System: ti.Ref, Request: mi.Ref, Args: rec, Key: reasonr.UUID(r.Key)}}, nil
}

// RequestDone deletes a request (port.RequestQueue).
func (s *Store) RequestDone(ctx context.Context, r *kind.LeasedRequest) error {
	rq, ok := s.db.(port.Requests)
	if !ok {
		return errNoPort("request queue")
	}
	return rq.RequestDone(ctx, r.Tenant, r.Request.Key.UUID())
}

// RetryRequest schedules the next attempt (port.RequestQueue).
func (s *Store) RetryRequest(ctx context.Context, r *kind.LeasedRequest, at time.Time, reason string) error {
	rq, ok := s.db.(port.Requests)
	if !ok {
		return errNoPort("request queue")
	}
	return rq.RetryRequest(ctx, r.Tenant, r.Request.Key.UUID(), at, reason)
}

// ExportState returns the live timers and counters of a tenant (port.StateTransfer).
func (s *Store) ExportState(ctx context.Context, tenant port.Tenant, a *reasonr.Arena) (kind.EngineState, error) {
	tr, ok := s.db.(port.Transfer)
	if !ok {
		return kind.EngineState{}, errNoPort("state transfer")
	}
	m, err := s.model(tenant)
	if err != nil {
		return kind.EngineState{}, err
	}
	ex, err := tr.ExportTimers(ctx, tenant)
	if err != nil {
		return kind.EngineState{}, err
	}
	st := kind.EngineState{Sequences: map[reasonr.TypeRef]int64{}}
	for name, v := range ex.Sequences {
		if ti := m.s.Lookup(name); ti != nil {
			st.Sequences[ti.Ref] = v
		}
	}
	for i := range ex.Timers {
		r := &ex.Timers[i]
		lt, err := s.timer(ctx, tenant, r, a)
		if err != nil {
			return st, err
		}
		var subjects []reasonr.ArgSubject
		if r.Sealed != nil {
			mi := &m.s.Type(lt.Entity).Methods[lt.Command]
			ss, err := storekit.DecodeSealed(mi.Params, r.Sealed)
			if err != nil {
				return st, err
			}
			if subjects, err = m.crypto.ArgSubjectsOf(a, ss); err != nil {
				return st, err
			}
		}
		st.Timers = append(st.Timers, kind.TimerState{ID: lt.ID, At: r.At, Offset: r.Offset, Entity: lt.Entity,
			EntityID: lt.EntityID, Command: lt.Command, Args: lt.Args, Subjects: subjects})
	}
	return st, nil
}

// ImportSequences raises the counters of a tenant (port.StateTransfer).
func (s *Store) ImportSequences(ctx context.Context, tenant port.Tenant, seqs map[reasonr.TypeRef]int64) error {
	tr, ok := s.db.(port.Transfer)
	if !ok {
		return errNoPort("state transfer")
	}
	m, err := s.model(tenant)
	if err != nil {
		return err
	}
	byName := map[string]int64{}
	for t, v := range seqs {
		if ti := m.s.Type(t); ti != nil {
			byName[ti.Name] = v
		}
	}
	return tr.ImportSequences(ctx, tenant, byName)
}

// ClearTimers deletes the timers of a tenant (port.StateTransfer).
func (s *Store) ClearTimers(ctx context.Context, tenant port.Tenant) error {
	tr, ok := s.db.(port.Transfer)
	if !ok {
		return errNoPort("state transfer")
	}
	return tr.ClearTimers(ctx, tenant)
}

// PutEntities writes entity states in a batch (port.EntitySink): a row is
// written only if its version is greater than the stored one.
func (s *Store) PutEntities(ctx context.Context, tenant port.Tenant, a *reasonr.Arena, rows []kind.EntityState) error {
	sink, ok := s.db.(port.RowSink)
	if !ok {
		return errNoPort("row sink")
	}
	if len(rows) == 0 {
		return nil
	}
	m, err := s.model(tenant)
	if err != nil {
		return err
	}
	seal := s.ring.Sealer(tenant)
	if m.personal {
		if err := seal.Ready(ctx); err != nil {
			return err
		}
	}
	writes := make([]port.RowWrite, 0, len(rows))
	for _, e := range rows {
		tb, err := m.table(e.Type)
		if err != nil {
			return err
		}
		row, err := m.encodeRow(ctx, seal, tb, a, e.ID, e.State.Value())
		if err != nil {
			return err
		}
		row.Version = e.Version
		writes = append(writes, port.RowWrite{Table: &tb.port, Row: row})
	}
	return sink.PutRows(ctx, tenant, writes)
}

// LastSchema returns the schema of the model written last (ports.SchemaStore).
func (s *Store) LastSchema(ctx context.Context, model string) (*reasonr.Schema, error) {
	ss, ok := s.db.(port.Schemas)
	if !ok {
		return nil, nil
	}
	data, err := ss.LastSchema(ctx, model)
	if err != nil || data == nil {
		return nil, err
	}
	return reasonr.ParseSchema(data)
}

// EnsureSchema brings the store to the model sc (ports.SchemaStore): the
// store creates what is missing; the rows of the tables whose key columns or
// personal fields changed are rewritten here.
func (s *Store) EnsureSchema(ctx context.Context, name string, sc *reasonr.Schema, tenants []port.Tenant) error {
	ss, ok := s.db.(port.Schemas)
	if !ok {
		return nil
	}
	m, err := s.modelOf(sc)
	if err != nil {
		return err
	}
	l, err := m.layout(name)
	if err != nil {
		return err
	}
	prev, err := s.LastSchema(ctx, name)
	if err != nil {
		return err
	}
	l.Rewrite, l.Replace = rewriteTables(m, prev)
	return ss.EnsureSchema(ctx, l, tenants, s.schemaRewriter())
}

// rewriteTables are the tables whose rows a schema change rewrites (a
// changed personal marking, a renamed personal field; without a previous
// schema — the tables with personal fields: data written before
// encryption) and the key columns that changed between blind and plain.
func rewriteTables(m *model, prev *reasonr.Schema) (rewrite, replace []string) {
	for _, t := range m.tables {
		if t == nil {
			continue
		}
		if prev == nil {
			if t.port.Personal {
				rewrite = append(rewrite, t.ti.Name)
			}
			continue
		}
		pt := prev.Lookup(t.ti.Name)
		if pt == nil || pt.Kind != t.ti.Kind {
			continue
		}
		old, err := layout(prev, pt)
		if err != nil || old.port.Personal != t.port.Personal || personalChanged(pt, t.ti) {
			rewrite = append(rewrite, t.ti.Name)
		}
		if err != nil {
			continue
		}
		for _, k := range t.port.Keys {
			for _, o := range old.port.Keys {
				if o.Name == k.Name && (o.Kind == port.ColBlind) != (k.Kind == port.ColBlind) {
					replace = append(replace, t.ti.Name+"."+k.Name)
				}
			}
		}
	}
	return rewrite, replace
}

// personalChanged reports whether a field became or stopped being personal,
// or was renamed with was.
func personalChanged(prev, cur *reasonr.TypeInfo) bool {
	for _, f := range cur.Fields {
		if f.Personal == nil {
			continue
		}
		if f.WasName != "" {
			return true // re-encrypted under the new name
		}
		pf := prev.Field(f.Name)
		if pf == nil || (pf.Personal == nil) != (f.Personal == nil) {
			return true
		}
	}
	for _, f := range prev.Fields {
		if f.Personal != nil {
			if cf := cur.Field(f.Name); cf != nil && cf.Personal == nil {
				return true
			}
		}
	}
	return false
}
