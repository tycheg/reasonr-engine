package rows

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/pii"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/internal/storekit"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Commit commits a unit of work (port.UnitStore): it lays the unit out in
// rows and records and the store writes them in one transaction.
func (s *Store) Commit(ctx context.Context, tenant port.Tenant, u *kind.Unit) (port.Position, error) {
	m, err := s.model(tenant)
	if err != nil {
		return 0, err
	}
	// New subject keys are written in the commit transaction, with the data.
	created := &newKeys{}
	ctx = context.WithValue(ctx, newKeysKey{}, created)
	seal := s.ring.Sealer(tenant)
	if m.personal && (u.Work != nil || u.Result != nil) {
		if err := seal.Ready(ctx); err != nil {
			return 0, err
		}
	}
	rec := &port.UnitRecord{Idempotency: u.Idempotency, Expires: u.Expires, Now: u.Now}
	if u.FiredTimer.Kind != reasonr.KindNone {
		id := u.FiredTimer.UUID()
		rec.Fired = &id
	}
	pc := m.payloadCrypto(seal)
	var views map[reasonr.TypeRef]*table
	if u.Work != nil {
		versions := map[int]uint64{}
		for i := range u.Work.Entities {
			ch := &u.Work.Entities[i]
			tb, err := m.table(ch.Type)
			if err != nil {
				return 0, err
			}
			row, err := m.encodeRow(ctx, seal, tb, u.Arena, ch.ID, ch.State.Value())
			if err != nil {
				return 0, err
			}
			row.Version = ch.Version
			rec.Writes = append(rec.Writes, port.RowWrite{Table: &tb.port, Row: row})
			versions[i] = ch.Version + 1
		}
		views = map[reasonr.TypeRef]*table{}
		if err := m.projectionRows(ctx, seal, u.Arena, u.Projections, rec, views); err != nil {
			return 0, err
		}
		if err := m.timerRecords(ctx, pc, u, rec); err != nil {
			return 0, err
		}
		if rec.Outbox, err = storekit.EncodePayload(ctx, m.json, pc, u, versions); err != nil {
			return 0, err
		}
		if rec.Requests, err = m.requestRecords(ctx, pc, u); err != nil {
			return 0, err
		}
		if len(u.Work.Forget) > 0 {
			if rec.Forget, err = storekit.Refs(m.s, u.Arena, u.Work.Forget); err != nil {
				return 0, err
			}
			rec.ForgetTables, rec.Rewrite = m.personalTables(nil), s.forgetRewriter(rec.Forget)
		}
	}
	if u.Idempotency != "" && u.Result != nil {
		// The response holds the unit's position, known in the transaction:
		// it is encoded there; encoding it here first creates the keys it
		// needs, so the key check covers them.
		if _, err := storekit.EncodeResult(ctx, m.json, pc, u.Arena, 0, u.Result); err != nil {
			return 0, err
		}
		rec.Result = func(pos port.Position) ([]byte, error) {
			return storekit.EncodeResult(ctx, m.json, pc, u.Arena, pos, u.Result)
		}
	}
	for _, kv := range seal.Used() {
		if !kv.Created { // a key the unit creates is written in its transaction
			rec.Keys = append(rec.Keys, port.KeyVersion{Subject: kv.Subject, Version: kv.Version})
		}
	}
	rec.NewKeys = created.keys
	pos, err := s.db.Commit(ctx, tenant, rec)
	if err != nil {
		// A key the commit found destroyed (a stale cache) or created by a
		// rolled back unit is read again on the retry.
		var used []port.SubjectRef
		for _, kv := range seal.Used() {
			used = append(used, kv.Subject)
		}
		s.ring.Forgotten(tenant, used)
		if ks, ok := s.keyStore(); ok && len(rec.NewKeys) > 0 {
			ks.conflicted(rec.NewKeys)
		}
		var uv *port.UniqueKeyViolation
		if errors.As(err, &uv) {
			return 0, m.uniqueViolation(u, uv)
		}
		return 0, err
	}
	if len(rec.Forget) > 0 {
		s.ring.Forgotten(tenant, rec.Forget)
	}
	return pos, nil
}

// uniqueViolation turns the violated index of the store into the kernel's
// violation with the key from the unit's record.
func (m *model) uniqueViolation(u *kind.Unit, uv *port.UniqueKeyViolation) error {
	ti := m.s.Lookup(uv.Type)
	if ti == nil {
		return uv
	}
	var in *reasonr.IndexInfo
	for i := range ti.Indexes {
		if ti.Indexes[i].Unique && Snake(ti.Indexes[i].Name) == uv.Index {
			in = &ti.Indexes[i]
		}
	}
	if in == nil {
		return uv
	}
	out := &ports.UniqueViolation{Type: ti.Ref, Index: in.Ref}
	if u.Work != nil {
		for _, ch := range u.Work.Entities {
			if ch.Type == ti.Ref {
				if key, ok := storekit.UniqueKey(u.Arena, ch.State.Value(), ch.ID, in); ok {
					out.Key = key
				}
			}
		}
	}
	return out
}

// projectionRows lays out the operations on synchronous projections: the
// unit's outcome for each row is its last operation; a row is written whole.
func (m *model) projectionRows(ctx context.Context, seal *pii.Sealer, a *reasonr.Arena, ops []reasonr.ProjectionOp,
	rec *port.UnitRecord, _ map[reasonr.TypeRef]*table) error {
	for _, op := range storekit.LastOps(a, ops) {
		tb, err := m.table(op.Projection)
		if err != nil {
			return err
		}
		if op.Kind == reasonr.ProjectionDelete {
			idp, err := m.idScalar(tb, a, op.ID)
			if err != nil {
				return err
			}
			key, err := m.json.Append(nil, a, reasonr.Type{Kind: reasonr.KindID, Ref: op.Projection}, op.ID)
			if err != nil {
				return err
			}
			rec.Deletes = append(rec.Deletes, port.RowDelete{Table: &tb.port, ID: idp, Key: string(key)})
			continue
		}
		state, err := storekit.ProjectionRow(m.rec, a, &op)
		if err != nil {
			return err
		}
		row, err := m.encodeRow(ctx, seal, tb, a, op.ID, state)
		if err != nil {
			return err
		}
		key, err := m.json.Append(nil, a, reasonr.Type{Kind: reasonr.KindID, Ref: op.Projection}, op.ID)
		if err != nil {
			return err
		}
		rec.Writes = append(rec.Writes, port.RowWrite{Table: &tb.port, Row: row, Projection: true, Key: string(key)})
	}
	return nil
}

// timerRecords lays out the timers to set and delete.
func (m *model) timerRecords(ctx context.Context, pc *storekit.PayloadCrypto, u *kind.Unit, rec *port.UnitRecord) error {
	a := u.Arena
	for i := range u.Work.Timers {
		op := &u.Work.Timers[i]
		if op.Kind == reasonr.TimerDelete {
			rec.DeleteTimers = append(rec.DeleteTimers, op.ID.UUID())
			continue
		}
		ti := m.s.Type(op.Entity)
		if ti == nil || int(op.Command) >= len(ti.Methods) {
			return fmt.Errorf("rows: a timer for an unknown command %d.%d", op.Entity, op.Command)
		}
		mi := &ti.Methods[op.Command]
		id, err := m.json.Append(nil, a, storekit.IDType(op.Entity), op.EntityID)
		if err != nil {
			return err
		}
		args := op.Args.Value()
		var sealed []byte
		if pc != nil {
			if args, sealed, err = pc.SealArgs(ctx, a, ti.Name+"."+mi.Name, mi.Params, args, op.Subjects); err != nil {
				return err
			}
		}
		data, err := m.json.AppendFields(nil, a, mi.Params, args.Record())
		if err != nil {
			return fmt.Errorf("rows: timer arguments of %s.%s: %w", ti.Name, mi.Name, err)
		}
		// At is by the real clock, Offset is the scenario clock offset (RFC §27.5).
		rec.Timers = append(rec.Timers, port.TimerRecord{ID: op.ID.UUID(), At: op.At.Add(-u.Offset), Offset: u.Offset,
			Entity: ti.Name, EntityID: id, Command: mi.Name, Args: data, Sealed: sealed})
	}
	return nil
}

// requestRecords lays out the external requests: the arguments without
// personal values and their ciphertexts.
func (m *model) requestRecords(ctx context.Context, pc *storekit.PayloadCrypto, u *kind.Unit) ([]port.RequestRecord, error) {
	a := u.Arena
	var out []port.RequestRecord
	for i := range u.Work.Requests {
		rq := &u.Work.Requests[i]
		ti := m.s.Type(rq.System)
		if ti == nil || int(rq.Request) >= len(ti.Methods) {
			return nil, fmt.Errorf("rows: a request to an unknown system %d.%d", rq.System, rq.Request)
		}
		mi := &ti.Methods[rq.Request]
		args := rq.Args.Value()
		var sealed []byte
		if pc != nil {
			var err error
			if args, sealed, err = pc.SealArgs(ctx, a, ti.Name+"."+mi.Name, mi.Params, args, rq.Subjects); err != nil {
				return nil, err
			}
		}
		data, err := m.json.AppendFields(nil, a, mi.Params, args.Record())
		if err != nil {
			return nil, fmt.Errorf("rows: request arguments of %s.%s: %w", ti.Name, mi.Name, err)
		}
		r := port.RequestRecord{Key: rq.Key.UUID(), System: ti.Name, Request: mi.Name, Args: data, Sealed: sealed}
		if t := mi.Timeout(a, rq.Args); t.Kind != reasonr.KindNone {
			id := t.UUID()
			r.Timer = &id
		}
		out = append(out, r)
	}
	return out, nil
}

// payloadCrypto encrypts the outbox, keyed responses, timer and request
// arguments; nil for a model without personal data.
func (m *model) payloadCrypto(seal *pii.Sealer) *storekit.PayloadCrypto {
	if !m.personal {
		return nil
	}
	return &storekit.PayloadCrypto{Crypto: m.crypto, Sealer: seal}
}

// Remembered returns the response stored by key (port.UnitStore).
func (s *Store) Remembered(ctx context.Context, tenant port.Tenant, key string, now time.Time, a *reasonr.Arena) (*kind.CommandResult, error) {
	m, err := s.model(tenant)
	if err != nil {
		return nil, err
	}
	data, err := s.db.Remembered(ctx, tenant, key, now)
	if err != nil || data == nil {
		return nil, err
	}
	return storekit.DecodeResult(ctx, m.json, m.payloadCrypto(s.ring.Sealer(tenant)), a, data)
}

// personalTables are the model's tables with personal values; only — of
// these types only (nil — all).
func (m *model) personalTables(only []reasonr.TypeRef) []*port.Table {
	var out []*port.Table
	for _, t := range m.tables {
		if t != nil && t.port.Personal && (only == nil || slices.Contains(only, t.ti.Ref)) {
			out = append(out, &t.port)
		}
	}
	return out
}

// keyStore returns the key store of the row store.
func (s *Store) keyStore() (*keyStore, bool) {
	ks, ok := s.keys.(*keyStore)
	return ks, ok
}
