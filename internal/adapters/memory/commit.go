package memory

import (
	"context"
	"fmt"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/pii"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/internal/storekit"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// pending is the state of a row after the unit of work, in the unit's arena.
type pending struct {
	t       reasonr.TypeRef
	key     string
	id      reasonr.Value
	state   reasonr.Value
	deleted bool
	version uint64 // the version at load (current for projections)
	events  []reasonr.Event
	data    []byte // the canonical encoding of state without personal values
	// Personal data (sealPending): the ciphertexts and blind values of the
	// state, event arguments without personal values (the encoding and the
	// values) and their ciphertexts.
	sealed     []storekit.Sealed
	blind      []blindKey
	evArgs     [][]byte
	evStripped []reasonr.Value
	evSealed   [][]storekit.Sealed
}

// timer is a scheduled timer: values in its own arena.
type timer struct {
	a          *reasonr.Arena
	id         reasonr.Value
	at         time.Time     // the moment by the real clock
	offset     time.Duration // the scenario clock offset (RFC §27.5): its moment is at + offset
	order      uint64        // the scheduling order
	entity     reasonr.TypeRef
	entityID   reasonr.Value
	command    reasonr.MethodRef
	args       reasonr.Record // without personal values
	sealed     []byte         // their ciphertexts (storekit.EncodeSealed)
	leaseUntil time.Time      // the lease: not selected until this moment
	attempts   int
	dead       string // the reason; "" — alive
}

// remembered is a response by idempotency key: canonical JSON
// (storekit.EncodeResult), personal arguments are ciphertexts.
type remembered struct {
	data    []byte
	expires time.Time
}

type rowKey struct {
	t   reasonr.TypeRef
	key string
}

// Commit commits a unit of work (port.UnitStore): it checks the
// idempotency key, the fired timer, versions and uniqueness, then applies
// states and events, synchronous projections, timers and forget, and writes
// the outbox and the response by key.
func (s *Store) Commit(ctx context.Context, tenant port.Tenant, u *kind.Unit) (port.Position, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.space(tenant)
	if err != nil {
		return 0, err
	}
	if u.Idempotency != "" {
		if r := sp.keys[u.Idempotency]; r != nil && r.expires.After(u.Now) {
			return 0, port.ErrDuplicateKey
		}
	}
	if u.FiredTimer.Kind != reasonr.KindNone && sp.timers[u.FiredTimer.UUID()] == nil {
		return 0, port.ErrTimerGone
	}
	var rows []*pending
	var timers []*timer
	var entry *outboxEntry
	seal := s.ring.Sealer(tenant)
	if u.Work != nil {
		if rows, err = sp.prepare(ctx, seal, u); err != nil {
			return 0, err
		}
		if timers, err = s.prepareTimers(ctx, seal, sp, u.Arena, u.Work.Timers, u.Offset); err != nil {
			return 0, err
		}
		if entry, err = s.outboxEntry(ctx, seal, sp, tenant, u, rows); err != nil {
			return 0, err
		}
		if err := s.checkKeys(tenant, seal); err != nil {
			return 0, err
		}
	}
	var result []byte
	if u.Idempotency != "" && u.Result != nil {
		pos := port.Position(0)
		if u.Work != nil {
			pos = s.position + 1
		}
		if result, err = storekit.EncodeResult(ctx, sp.json, &storekit.PayloadCrypto{Crypto: sp.crypto, Sealer: seal}, u.Arena, pos, u.Result); err != nil {
			return 0, err
		}
	}

	// The checks passed: apply.
	var pos port.Position
	if u.Work != nil {
		s.position++
		pos = s.position
		for _, p := range rows {
			if err := sp.apply(u.Arena, p, pos); err != nil {
				return 0, err
			}
		}
		for i, op := range u.Work.Timers {
			if op.Kind == reasonr.TimerDelete {
				delete(sp.timers, op.ID.UUID())
				continue
			}
			sp.timers[op.ID.UUID()] = timers[i]
		}
	}
	if u.FiredTimer.Kind != reasonr.KindNone {
		delete(sp.timers, u.FiredTimer.UUID())
	}
	if u.Work != nil {
		if len(u.Work.Forget) > 0 {
			refs, err := s.forget(tenant, sp, u.Arena, u.Work.Forget)
			if err != nil {
				return 0, err
			}
			defer s.ring.Forgotten(tenant, refs)
		}
		entry.unit.Position = pos
		s.outbox = append(s.outbox, *entry)
		s.enqueue(sp, entry)
		close(s.wake)
		s.wake = make(chan struct{})
	}
	if result != nil {
		sp.remember(u, result)
	}
	return pos, nil
}

// prepare checks entity versions, applies the operations on synchronous
// projections to the pending row states, encodes the states and checks
// uniqueness.
func (sp *space) prepare(ctx context.Context, seal *pii.Sealer, u *kind.Unit) ([]*pending, error) {
	w, a := u.Work, u.Arena
	var rows []*pending
	byKey := map[rowKey]*pending{}
	for i := range w.Entities {
		ch := &w.Entities[i]
		r, k, err := sp.row(a, ch.Type, ch.ID)
		if err != nil {
			return nil, err
		}
		version := uint64(0)
		if r != nil {
			version = r.version
		}
		if version != ch.Version || (r != nil) != ch.Existed {
			return nil, port.ErrConflict
		}
		p := &pending{t: ch.Type, key: k, id: ch.ID, state: ch.State.Value(), version: version, events: ch.Events}
		rows = append(rows, p)
		byKey[rowKey{ch.Type, k}] = p
	}
	for i := range u.Projections {
		if err := sp.project(a, &u.Projections[i], byKey, &rows); err != nil {
			return nil, err
		}
	}
	for _, p := range rows {
		if p.deleted {
			continue
		}
		if err := sp.sealPending(ctx, seal, a, p); err != nil {
			return nil, err
		}
	}
	for _, p := range rows {
		if uv := sp.unique(a, p, byKey); uv != nil {
			return nil, uv
		}
	}
	return rows, nil
}

// project applies a projection operation to the pending state of a row:
// the row is written whole, the previous state is not needed.
func (sp *space) project(a *reasonr.Arena, op *reasonr.ProjectionOp, byKey map[rowKey]*pending, rows *[]*pending) error {
	r, k, err := sp.row(a, op.Projection, op.ID)
	if err != nil {
		return err
	}
	p := byKey[rowKey{op.Projection, k}]
	if p == nil {
		p = &pending{t: op.Projection, key: k, id: op.ID}
		if r != nil {
			p.version = r.version
		}
		byKey[rowKey{op.Projection, k}] = p
		*rows = append(*rows, p)
	}
	if op.Kind == reasonr.ProjectionDelete {
		p.deleted = true
		return nil
	}
	state, err := storekit.ProjectionRow(sp.codec, a, op)
	if err != nil {
		return err
	}
	p.state, p.deleted = state, false
	return nil
}

// unique checks the unique indexes of the type of row p by the state after
// the unit of work. A key with none does not take part (RFC §10.2).
func (sp *space) unique(a *reasonr.Arena, p *pending, byKey map[rowKey]*pending) *ports.UniqueViolation {
	if p.deleted {
		return nil
	}
	ti := sp.schema.Type(p.t)
	for ix := range ti.Indexes {
		in := &ti.Indexes[ix]
		if !in.Unique {
			continue
		}
		key, ok := storekit.UniqueKey(a, p.state, p.id, in)
		if !ok {
			continue
		}
		uk, ok, err := sp.uniqueKey(a, p.t, in, p.state, p.id, p.blind)
		if err != nil || !ok {
			return &ports.UniqueViolation{Type: p.t, Index: in.Ref, Key: key}
		}
		clash := func(ra *reasonr.Arena, state, id reasonr.Value) bool {
			for i, k := range in.Keys {
				if !reasonr.Equal(a, key[i], ra, storekit.At(ra, state, id, k.Field)) {
					return false
				}
			}
			return true
		}
		taken := false
		// Rows this unit of work changes — by the new state; the others — by
		// the uniqueness index.
		for _, q := range byKey {
			if q != p && q.t == p.t && !q.deleted && clash(a, q.state, q.id) {
				taken = true
			}
		}
		if !taken {
			if owner, ok := sp.uniq[uk]; ok && owner != p.key && byKey[rowKey{p.t, owner}] == nil {
				taken = true
			}
		}
		if taken {
			return &ports.UniqueViolation{Type: p.t, Index: in.Ref, Key: key}
		}
	}
	return nil
}

// prepareTimers encodes the arguments of scheduled timers into their
// arenas; personal ones are ciphertexts.
func (s *Store) prepareTimers(ctx context.Context, seal *pii.Sealer, sp *space, a *reasonr.Arena, ops []reasonr.TimerOp, offset time.Duration) ([]*timer, error) {
	out := make([]*timer, len(ops))
	pc := &storekit.PayloadCrypto{Crypto: sp.crypto, Sealer: seal}
	for i := range ops {
		op := &ops[i]
		if op.Kind != reasonr.TimerSchedule {
			continue
		}
		ti := sp.schema.Type(op.Entity)
		if ti == nil || int(op.Command) >= len(ti.Methods) {
			return nil, fmt.Errorf("memory: a timer for an unknown command %d.%d", op.Entity, op.Command)
		}
		mi := &ti.Methods[op.Command]
		args, sealed, err := pc.SealArgs(ctx, a, ti.Name+"."+mi.Name, mi.Params, op.Args.Value(), op.Subjects)
		if err != nil {
			return nil, err
		}
		data, err := sp.json.AppendFields(nil, a, mi.Params, args.Record())
		if err != nil {
			return nil, fmt.Errorf("memory: timer arguments of %s.%s: %w", ti.Name, mi.Name, err)
		}
		ta := reasonr.NewArena()
		rec, err := sp.json.DecodeFields(ta, 0, mi.Params, data)
		if err != nil {
			return nil, err
		}
		s.timerSeq++
		out[i] = &timer{
			a: ta, id: ta.Copy(a, op.ID), at: op.At.Add(-offset), offset: offset, order: s.timerSeq,
			entity: op.Entity, entityID: ta.Copy(a, op.EntityID), command: op.Command, args: rec, sealed: sealed,
		}
	}
	return out, nil
}

// apply writes the row state and the entity events.
func (sp *space) apply(a *reasonr.Arena, p *pending, pos port.Position) error {
	tab := sp.rows[p.t]
	if tab == nil {
		tab = map[string]*row{}
		sp.rows[p.t] = tab
	}
	if p.deleted {
		if r := tab[p.key]; r != nil {
			sp.unindex(r)
			r.sealed, r.blind = nil, nil
			sp.setSubjects(r)
		}
		delete(tab, p.key)
		return nil
	}
	r := tab[p.key]
	if r == nil {
		r = &row{t: p.t, a: reasonr.NewArena(), key: p.key}
		tab[p.key] = r
	}
	for i, ev := range p.events {
		r.events = append(r.events, storedEvent{Position: pos, Name: ev.Name, Args: p.evArgs[i], Sealed: p.evSealed[i]})
	}
	return sp.store(r, a, p.id, p.data, p.version+1, p.sealed, p.blind)
}

// store writes the state data (without personal values), its ciphertexts,
// blind values and the identifier id (from the arena src) into the row
// arena.
func (sp *space) store(r *row, src *reasonr.Arena, id reasonr.Value, data []byte, version uint64, sealed []storekit.Sealed, blind []blindKey) error {
	na := reasonr.NewArena()
	state, err := sp.codec.DecodeRecord(na, r.t, data)
	if err != nil {
		return fmt.Errorf("memory: the state of %s cannot be read: %w", sp.typeName(r.t), err)
	}
	r.a, r.id, r.state, r.data, r.version = na, na.Copy(src, id), state.Value(), data, version
	r.sealed, r.blind = sealed, blind
	sp.setSubjects(r)
	return sp.index(r)
}

// uniqueKey is the key value of the unique index in of the record (state,
// id): for personal keys — the blind values blind. ok == false — the key
// has none and does not take part in uniqueness (RFC §10.2).
func (sp *space) uniqueKey(a *reasonr.Arena, t reasonr.TypeRef, in *reasonr.IndexInfo, state, id reasonr.Value, blind []blindKey) (uniqKey, bool, error) {
	var b []byte
	for _, k := range in.Keys {
		if sp.crypto.Personal(t, k.Field) {
			v, ok := blindOf(blind, k.Field)
			if !ok {
				return uniqKey{}, false, nil
			}
			b = append(b, v...)
			continue
		}
		v := storekit.At(a, state, id, k.Field)
		if v.Kind == reasonr.KindNone {
			return uniqKey{}, false, nil
		}
		ft, err := storekit.FieldType(sp.schema, t, k.Field)
		if err != nil {
			return uniqKey{}, false, err
		}
		if b, err = sp.codec.Append(b, a, ft, v); err != nil {
			return uniqKey{}, false, err
		}
	}
	return uniqKey{t: t, ix: in.Ref, key: string(b)}, true, nil
}

// index puts the values of a row's unique indexes into the uniqueness
// index, removing the previous ones.
func (sp *space) index(r *row) error {
	sp.unindex(r)
	ti := sp.schema.Type(r.t)
	for ix := range ti.Indexes {
		in := &ti.Indexes[ix]
		if !in.Unique {
			continue
		}
		uk, ok, err := sp.uniqueKey(r.a, r.t, in, r.state, r.id, r.blind)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		sp.uniq[uk] = r.key
		r.uniq = append(r.uniq, uk)
	}
	return nil
}

// unindex removes the values of a row's unique indexes.
func (sp *space) unindex(r *row) {
	for _, uk := range r.uniq {
		if sp.uniq[uk] == r.key {
			delete(sp.uniq, uk)
		}
	}
	r.uniq = r.uniq[:0]
}

// copyOps copies projection operations from the arena src into a.
func copyOps(a, src *reasonr.Arena, ops []reasonr.ProjectionOp) []reasonr.ProjectionOp {
	if len(ops) == 0 {
		return nil
	}
	out := make([]reasonr.ProjectionOp, len(ops))
	for i, op := range ops {
		op.ID = a.Copy(src, op.ID)
		set := make([]reasonr.FieldValue, len(op.Set))
		for j, fv := range op.Set {
			set[j] = reasonr.FieldValue{Field: fv.Field, Value: a.Copy(src, fv.Value)}
		}
		op.Set = set
		out[i] = op
	}
	return out
}

// remember stores the response by the idempotency key of the unit u.
func (sp *space) remember(u *kind.Unit, result []byte) {
	if len(sp.keys) >= 10000 {
		for k, r := range sp.keys {
			if !r.expires.After(u.Now) {
				delete(sp.keys, k)
			}
		}
	}
	sp.keys[u.Idempotency] = &remembered{data: result, expires: u.Expires}
}

// Remembered returns the response stored by key (port.UnitStore): the
// personal arguments of forgotten subjects are none.
func (s *Store) Remembered(ctx context.Context, tenant port.Tenant, key string, now time.Time, a *reasonr.Arena) (*kind.CommandResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.space(tenant)
	if err != nil {
		return nil, err
	}
	r := sp.keys[key]
	if r == nil || !r.expires.After(now) {
		return nil, nil
	}
	return storekit.DecodeResult(ctx, sp.json, &storekit.PayloadCrypto{Crypto: sp.crypto, Sealer: s.ring.Sealer(tenant)}, a, r.data)
}

// PutEntities writes the entity states whose version is greater than the
// stored one (kind.EntitySink): the entity database of an engine kind that
// keeps its entities in memory.
func (s *Store) PutEntities(ctx context.Context, tenant port.Tenant, a *reasonr.Arena, rows []kind.EntityState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.space(tenant)
	if err != nil {
		return err
	}
	seal := s.ring.Sealer(tenant)
	for _, e := range rows {
		r, k, err := sp.row(a, e.Type, e.ID)
		if err != nil {
			return err
		}
		if (r != nil && r.version >= e.Version) || e.Version == 0 {
			continue
		}
		p := &pending{t: e.Type, key: k, id: e.ID, state: e.State.Value(), version: e.Version - 1}
		if err := sp.sealPending(ctx, seal, a, p); err != nil {
			return err
		}
		if err := sp.apply(a, p, 0); err != nil {
			return err
		}
	}
	return nil
}
