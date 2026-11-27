// Package storekit holds the shared parts of the engine stores (RFC
// §30–31): values by path in a record, search order and conditions, page
// cursors, projection operations, unique index keys, erasing personal data
// on forget. The store adapters (memory, PostgreSQL) use them to behave
// identically — this is what the conformance tests check.
package storekit

import (
	"encoding/base64"
	"fmt"

	reasonr "github.com/tycheg/reasonr-compiler"
)

// At returns the value at a path in the record state; IDField is the
// identifier id. An intermediate none gives none.
func At(a *reasonr.Arena, state, id reasonr.Value, path reasonr.FieldPath) reasonr.Value {
	v := state
	for _, f := range path {
		if f == reasonr.IDField {
			v = id
			continue
		}
		if v.Kind != reasonr.KindRecord || int(f) >= int(v.Ref.Len) {
			return reasonr.None
		}
		v = a.Elems(v)[f]
	}
	return v
}

// FieldType returns the type of the value at a path in a record of type t
// as optional: intermediate fields may be none.
func FieldType(s *reasonr.Schema, t reasonr.TypeRef, path reasonr.FieldPath) (reasonr.Type, error) {
	typ := reasonr.Type{Kind: reasonr.KindRecord, Ref: t}
	for _, f := range path {
		if f == reasonr.IDField {
			typ = reasonr.Type{Kind: reasonr.KindID, Ref: typ.Ref}
			continue
		}
		ti := s.Type(typ.Ref)
		if typ.Kind != reasonr.KindRecord || ti == nil || int(f) >= len(ti.Fields) {
			return reasonr.Type{}, fmt.Errorf("storekit: a wrong path %v in type %d", path, t)
		}
		typ = ti.Fields[f].Type
	}
	typ.Optional = true
	return typ, nil
}

// SortKeys returns the search order keys: the query's sort, otherwise the
// index keys; then always the identifier.
func SortKeys(s *reasonr.Schema, q *reasonr.FindSpec) ([]reasonr.SortKey, error) {
	ti := s.Type(q.Type)
	if ti == nil {
		return nil, fmt.Errorf("storekit: no type %d", q.Type)
	}
	if len(q.Sort) > 0 || q.Index == reasonr.NoIndex {
		return q.Sort, nil
	}
	if int(q.Index) >= len(ti.Indexes) {
		return nil, fmt.Errorf("storekit: no index %d of %s", q.Index, ti.Name)
	}
	return ti.Indexes[q.Index].Keys, nil
}

// Matches checks the search conditions for the record state with the
// identifier id (arena ra); the condition values are in the arena a.
// Ordering comparisons with none are false, as in SQL.
func Matches(ra *reasonr.Arena, state, id reasonr.Value, a *reasonr.Arena, where []reasonr.Cond) bool {
	for _, c := range where {
		v := At(ra, state, id, c.Field)
		switch c.Op {
		case reasonr.OpEq:
			if !reasonr.Equal(ra, v, a, c.Value) {
				return false
			}
		case reasonr.OpNe:
			if reasonr.Equal(ra, v, a, c.Value) {
				return false
			}
		case reasonr.OpIn:
			in := false
			for _, x := range a.Elems(c.Value) {
				if reasonr.Equal(ra, v, a, x) {
					in = true
					break
				}
			}
			if !in {
				return false
			}
		default:
			if v.Kind == reasonr.KindNone || c.Value.Kind == reasonr.KindNone {
				return false
			}
			cmp := reasonr.Compare(ra, v, a, c.Value)
			ok := c.Op == reasonr.OpLt && cmp < 0 || c.Op == reasonr.OpLe && cmp <= 0 ||
				c.Op == reasonr.OpGt && cmp > 0 || c.Op == reasonr.OpGe && cmp >= 0
			if !ok {
				return false
			}
		}
	}
	return true
}

// Row is a record for order comparison: the state and the identifier in an arena.
type Row struct {
	A         *reasonr.Arena
	State, ID reasonr.Value
}

// Compare orders records by the keys keys, then by id: none is less than
// any value, desc reverses a key's order.
func Compare(x, y Row, keys []reasonr.SortKey) int {
	for _, k := range keys {
		c := reasonr.Compare(x.A, At(x.A, x.State, x.ID, k.Field), y.A, At(y.A, y.State, y.ID, k.Field))
		if k.Desc {
			c = -c
		}
		if c != 0 {
			return c
		}
	}
	return reasonr.Compare(x.A, x.ID, y.A, y.ID)
}

// AfterCursor compares a record with the cursor position pos (key values
// and id in the arena a): > 0 — the record is after the cursor.
func AfterCursor(x Row, keys []reasonr.SortKey, a *reasonr.Arena, pos []reasonr.Value) int {
	for i, k := range keys {
		c := reasonr.Compare(x.A, At(x.A, x.State, x.ID, k.Field), a, pos[i])
		if k.Desc {
			c = -c
		}
		if c != 0 {
			return c
		}
	}
	return reasonr.Compare(x.A, x.ID, a, pos[len(keys)])
}

// EncodeCursor is the cursor after the record x of type t: the values of
// the keys keys and id in the canonical binary encoding (runtime/record),
// base64url. The format is shared by every store.
func EncodeCursor(c *reasonr.RecordCodec, t reasonr.TypeRef, x Row, keys []reasonr.SortKey) (string, error) {
	b := appendUvarint(nil, uint64(len(keys)+1)) // as a record: the number of fields, the fields
	for _, k := range keys {
		ft, err := FieldType(c.Schema(), t, k.Field)
		if err != nil {
			return "", err
		}
		if b, err = c.Append(b, x.A, ft, At(x.A, x.State, x.ID, k.Field)); err != nil {
			return "", err
		}
	}
	b, err := c.Append(b, x.A, reasonr.Type{Kind: reasonr.KindID, Ref: t}, x.ID)
	return base64.RawURLEncoding.EncodeToString(b), err
}

// DecodeCursor decodes a cursor into the key values and id in the arena a.
func DecodeCursor(c *reasonr.RecordCodec, a *reasonr.Arena, t reasonr.TypeRef, keys []reasonr.SortKey, text []byte) ([]reasonr.Value, error) {
	b, err := base64.RawURLEncoding.DecodeString(string(text))
	if err != nil {
		return nil, fmt.Errorf("storekit: a wrong cursor: %w", err)
	}
	fields := make([]reasonr.FieldInfo, 0, len(keys)+1)
	for i, k := range keys {
		ft, err := FieldType(c.Schema(), t, k.Field)
		if err != nil {
			return nil, err
		}
		fields = append(fields, reasonr.FieldInfo{Ref: reasonr.FieldRef(i), Type: ft})
	}
	fields = append(fields, reasonr.FieldInfo{Ref: reasonr.FieldRef(len(keys)), Type: reasonr.Type{Kind: reasonr.KindID, Ref: t}})
	rec, err := c.DecodeFields(a, 0, fields, b)
	if err != nil {
		return nil, fmt.Errorf("storekit: a wrong cursor: %w", err)
	}
	return a.Fields(rec), nil
}

func appendUvarint(b []byte, x uint64) []byte {
	for x >= 0x80 {
		b = append(b, byte(x)|0x80)
		x >>= 7
	}
	return append(b, byte(x))
}

// ProjectionRow builds the state of a projection row by the record op in
// the arena a: a row is written whole (RFC §16) — the passed fields from
// op.Set, the rest by default; the previous row state does not take part.
func ProjectionRow(c *reasonr.RecordCodec, a *reasonr.Arena, op *reasonr.ProjectionOp) (reasonr.Value, error) {
	def, err := c.Default(a, op.Projection)
	if err != nil {
		return reasonr.None, err
	}
	m := a.Mark()
	for _, f := range a.Elems(def.Value()) {
		a.Push(f)
	}
	for _, fv := range op.Set {
		if int(fv.Field) >= a.Mark()-m {
			a.Drop(m)
			return reasonr.None, fmt.Errorf("storekit: no field %d in projection %d", fv.Field, op.Projection)
		}
		a.SetStack(m+int(fv.Field), fv.Value)
	}
	return a.End(reasonr.KindRecord, op.Projection, m), nil
}

// Own copies the projection operations and the subjects of a unit from the
// arena src into a new arena: a consumer of the effect stream that builds
// rows writes to the arena, and the unit's arena is read by the other
// consumers at the same time.
func Own(src *reasonr.Arena, ops []reasonr.ProjectionOp, subjects []reasonr.Subject) (*reasonr.Arena, []reasonr.ProjectionOp, []reasonr.Subject) {
	a := reasonr.NewArena()
	a.SetStatic(src.Static())
	outOps := make([]reasonr.ProjectionOp, len(ops))
	for i, op := range ops {
		op.ID = a.Copy(src, op.ID)
		if len(op.Set) > 0 {
			set := make([]reasonr.FieldValue, len(op.Set))
			for j, fv := range op.Set {
				set[j] = reasonr.FieldValue{Field: fv.Field, Value: a.Copy(src, fv.Value)}
			}
			op.Set = set
		}
		outOps[i] = op
	}
	var outSubjects []reasonr.Subject
	if len(subjects) > 0 {
		outSubjects = make([]reasonr.Subject, len(subjects))
		for i, sub := range subjects {
			outSubjects[i] = reasonr.Subject{Type: sub.Type, ID: a.Copy(src, sub.ID)}
		}
	}
	return a, outOps, outSubjects
}

// LastOps keeps of a unit's operations the last one on each row (ordered by
// the first operation on the row): a row is written whole, so the unit's
// result is its last write or deletion.
func LastOps(a *reasonr.Arena, ops []reasonr.ProjectionOp) []reasonr.ProjectionOp {
	out := make([]reasonr.ProjectionOp, 0, len(ops))
	byHash := map[uint64][]int{} // the row hash → its numbers in out
next:
	for _, op := range ops {
		h := a.Hash(op.ID)*31 + uint64(op.Projection)
		for _, i := range byHash[h] {
			if out[i].Projection == op.Projection && reasonr.Equal(a, out[i].ID, a, op.ID) {
				out[i] = op
				continue next
			}
		}
		byHash[h] = append(byHash[h], len(out))
		out = append(out, op)
	}
	return out
}

// UniqueKey returns the key values of the unique index in of a record; ok
// == false — one of the keys is none: the record does not take part in
// uniqueness (RFC §10.2).
func UniqueKey(a *reasonr.Arena, state, id reasonr.Value, in *reasonr.IndexInfo) ([]reasonr.Value, bool) {
	key := make([]reasonr.Value, len(in.Keys))
	for i, k := range in.Keys {
		key[i] = At(a, state, id, k.Field)
		if key[i].Kind == reasonr.KindNone {
			return nil, false
		}
	}
	return key, true
}

// Subject returns the subject of the personal field p of a record: the
// record itself or the identifier at the path p.Subject; ok == false — no subject (none).
func Subject(a *reasonr.Arena, state, id reasonr.Value, p *reasonr.Personal) (reasonr.TypeRef, reasonr.Value, bool) {
	v := id
	if len(p.Subject) > 0 {
		v = At(a, state, id, p.Subject)
	}
	if v.Kind != reasonr.KindID {
		return 0, reasonr.None, false
	}
	t := p.SubjectType
	if t == 0 {
		t = v.Type
	}
	return t, v, true
}
