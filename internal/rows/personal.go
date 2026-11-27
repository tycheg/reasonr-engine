package rows

import (
	"context"
	"fmt"
	"slices"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/pii"
	"github.com/tycheg/reasonr-engine/internal/storekit"
	"github.com/tycheg/reasonr-engine/port"
)

// Personal data in rows (RFC §20): the state holds a record where the
// personal fields are none; their ciphertexts are in Row.Sealed
// (storekit.EncodeSealed: the field, the subjects, the data), the row's
// subjects in Row.Subjects (forget finds rows by them), the index key
// columns on personal fields hold blind values.

// encodeRow makes the row of the record state with the identifier id of
// table t: the state without personal values, their ciphertexts, the key
// columns.
func (m *model) encodeRow(ctx context.Context, seal *pii.Sealer, t *table, a *reasonr.Arena, id, state reasonr.Value) (port.Row, error) {
	var row port.Row
	idp, err := m.idScalar(t, a, id)
	if err != nil {
		return row, err
	}
	row.ID = idp
	stored := state
	if t.port.Personal {
		stripped, sealed, err := m.crypto.Seal(ctx, seal, a, t.ti.Name, t.ti.Fields, state, m.crypto.SubjectsOf(a, t.ti.Fields, state, id))
		if err != nil {
			return row, err
		}
		stored = stripped
		if row.Sealed, err = storekit.EncodeSealed(t.ti.Fields, sealed); err != nil {
			return row, err
		}
		row.Subjects = storekit.SubjectRefs(sealed)
	}
	if row.State, err = m.json.AppendRecord(nil, a, stored.Record()); err != nil {
		return row, fmt.Errorf("rows: state of %s: %w", t.ti.Name, err)
	}
	row.Keys = make([]port.Scalar, len(t.keys))
	for i := range t.keys {
		k := &t.keys[i]
		v := storekit.At(a, state, id, k.path)
		if k.Kind != port.ColBlind {
			if row.Keys[i], err = m.scalar(a, k.Kind, k.typ, v); err != nil {
				return row, err
			}
			continue
		}
		if v.Kind == reasonr.KindNone {
			continue
		}
		b, err := m.crypto.Blind(ctx, seal, a, t.ti.Ref, k.path, v)
		if err != nil {
			return row, err
		}
		row.Keys[i] = []byte(b)
	}
	return row, nil
}

// decoded is a row read into an arena.
type decoded struct {
	storekit.Row
	version uint64
	sealed  []byte
}

// decodeRows reads rows of table t into the arena a and decrypts their
// personal values (the subject keys load with one store call).
func (m *model) decodeRows(ctx context.Context, seal *pii.Sealer, t *table, a *reasonr.Arena, rows []port.Row) ([]decoded, error) {
	out := make([]decoded, len(rows))
	all := make([][]storekit.Sealed, len(rows))
	for i, r := range rows {
		id, err := m.idValue(t, a, r.ID)
		if err != nil {
			return nil, err
		}
		rec, err := m.json.DecodeRecord(a, t.ti.Ref, r.State)
		if err != nil {
			return nil, fmt.Errorf("rows: state of %s: %w", t.ti.Name, err)
		}
		out[i] = decoded{Row: storekit.Row{A: a, State: rec.Value(), ID: id}, version: r.Version, sealed: r.Sealed}
		if len(r.Sealed) > 0 {
			if all[i], err = storekit.DecodeSealed(t.ti.Fields, r.Sealed); err != nil {
				return nil, fmt.Errorf("rows: %s: %w", t.ti.Name, err)
			}
		}
	}
	if err := storekit.Prefetch(ctx, seal, all...); err != nil {
		return nil, err
	}
	for i, ss := range all {
		if len(ss) == 0 {
			continue
		}
		if err := m.crypto.Open(ctx, seal, m.arenaOf(out[i]), t.ti.Name, t.ti.Fields, out[i].State, ss); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (m *model) arenaOf(d decoded) *reasonr.Arena { return d.A }

// forgetRewriter erases the ciphertexts and blind values of the subjects
// refs in a row (the second and third steps of forget, RFC §20): it works on
// the ciphertexts without decrypting anything.
func (s *Store) forgetRewriter(refs []port.SubjectRef) port.Rewriter {
	return func(ctx context.Context, tenant port.Tenant, pt *port.Table, r port.Row) (port.Row, bool, error) {
		m, err := s.model(tenant)
		if err != nil {
			return r, false, err
		}
		t := m.byName[pt.Type]
		if t == nil || len(r.Sealed) == 0 {
			return r, false, nil
		}
		ss, err := storekit.DecodeSealed(t.ti.Fields, r.Sealed)
		if err != nil {
			return r, false, fmt.Errorf("rows: %s: %w", t.ti.Name, err)
		}
		keep := ss[:0]
		for _, sv := range ss {
			if !slices.ContainsFunc(sv.Subjects, func(x port.SubjectRef) bool { return slices.Contains(refs, x) }) {
				keep = append(keep, sv)
			}
		}
		if len(keep) == len(ss) {
			return r, false, nil
		}
		out := r
		if out.Sealed, err = storekit.EncodeSealed(t.ti.Fields, keep); err != nil {
			return r, false, err
		}
		out.Subjects = storekit.SubjectRefs(keep)
		out.Keys = slices.Clone(r.Keys)
		for i := range t.keys {
			k := &t.keys[i]
			if k.Kind == port.ColBlind && i < len(out.Keys) && !slices.ContainsFunc(keep, func(sv storekit.Sealed) bool { return sv.Field == int(k.path[0]) }) {
				out.Keys[i] = nil
			}
		}
		return out, true, nil
	}
}

// schemaRewriter brings a stored row to the current model (a schema
// change): it fills new key columns and, when the personal data storage
// differs from the model (storekit.NeedsReseal), encrypts, decrypts or
// re-encrypts the personal values.
func (s *Store) schemaRewriter() port.Rewriter {
	return func(ctx context.Context, tenant port.Tenant, pt *port.Table, r port.Row) (port.Row, bool, error) {
		m, err := s.model(tenant)
		if err != nil {
			return r, false, err
		}
		t := m.byName[pt.Type]
		if t == nil {
			return r, false, nil
		}
		a := reasonr.GetArena()
		defer reasonr.PutArena(a)
		stored, err := m.json.DecodeRecord(a, t.ti.Ref, r.State)
		if err != nil {
			return r, false, fmt.Errorf("rows: state of %s: %w", t.ti.Name, err)
		}
		reseal, err := storekit.NeedsReseal(a, t.ti.Fields, stored.Value(), r.Sealed)
		if err != nil {
			return r, false, err
		}
		seal := s.ring.Sealer(tenant)
		rows, err := m.decodeRows(ctx, seal, t, a, []port.Row{r})
		if err != nil {
			return r, false, err
		}
		out, err := m.encodeRow(ctx, seal, t, a, rows[0].ID, rows[0].State)
		if err != nil {
			return r, false, err
		}
		out.ID, out.Version = r.ID, r.Version
		switch {
		case reseal:
			return out, true, nil
		case !sameKeys(out.Keys, r.Keys):
			// Only the key columns: the state and the ciphertexts stay.
			keys := out.Keys
			out = r
			out.Keys = keys
			return out, true, nil
		}
		return r, false, nil
	}
}

func sameKeys(x, y []port.Scalar) bool {
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if fmt.Sprintf("%v", x[i]) != fmt.Sprintf("%v", y[i]) {
			return false
		}
	}
	return true
}
