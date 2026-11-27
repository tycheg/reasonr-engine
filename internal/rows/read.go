package rows

import (
	"context"
	"fmt"
	"sort"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/storekit"
	"github.com/tycheg/reasonr-engine/port"
)

// Load returns an entity or projection record (port.Reader).
func (s *Store) Load(ctx context.Context, tenant port.Tenant, a *reasonr.Arena, t reasonr.TypeRef, id reasonr.Value) (reasonr.Record, uint64, bool, error) {
	m, err := s.model(tenant)
	if err != nil {
		return reasonr.Record{}, 0, false, err
	}
	tb, err := m.table(t)
	if err != nil {
		return reasonr.Record{}, 0, false, err
	}
	idp, err := m.idScalar(tb, a, id)
	if err != nil {
		return reasonr.Record{}, 0, false, err
	}
	r, ok, err := s.db.Load(ctx, tenant, &tb.port, idp)
	if err != nil {
		return reasonr.Record{}, 0, false, err
	}
	if !ok {
		def, err := m.rec.Default(a, t)
		return def, 0, false, err
	}
	rows, err := m.decodeRows(ctx, s.ring.Sealer(tenant), tb, a, []port.Row{r})
	if err != nil {
		return reasonr.Record{}, 0, false, err
	}
	return rows[0].State.Record(), r.Version, true, nil
}

// NextSequence allocates n sequence numbers (port.UnitStore).
func (s *Store) NextSequence(ctx context.Context, tenant port.Tenant, t reasonr.TypeRef, n int) (int64, error) {
	m, err := s.model(tenant)
	if err != nil {
		return 0, err
	}
	ti := m.s.Type(t)
	if ti == nil {
		return 0, fmt.Errorf("rows: no type %d", t)
	}
	return s.db.NextSequence(ctx, tenant, ti.Name, n)
}

// Find runs a search (port.Reader): conditions and the order go to the
// store over key columns; a page is keyset pagination with the same cursor
// as in memory. If a condition or a sort key is not on a column, every row
// of the type is read and filtered and sorted here.
func (s *Store) Find(ctx context.Context, tenant port.Tenant, a *reasonr.Arena, q *reasonr.FindSpec) (reasonr.Rows, error) {
	return find(ctx, s, s.db, tenant, a, q)
}

// find searches the reader db.
func find(ctx context.Context, s *Store, db port.RowReader, tenant port.Tenant, a *reasonr.Arena, q *reasonr.FindSpec) (reasonr.Rows, error) {
	m, err := s.model(tenant)
	if err != nil {
		return reasonr.Rows{}, err
	}
	tb, err := m.table(q.Type)
	if err != nil {
		return reasonr.Rows{}, err
	}
	keys, err := storekit.SortKeys(m.s, q)
	if err != nil {
		return reasonr.Rows{}, err
	}
	var cursor []reasonr.Value
	if q.Page && q.Cursor.Kind == reasonr.KindString {
		if cursor, err = storekit.DecodeCursor(m.rec, a, q.Type, keys, a.Data(q.Cursor)); err != nil {
			return reasonr.Rows{}, err
		}
	}
	seal := s.ring.Sealer(tenant)
	where := q.Where
	var blind []storekit.BlindCond
	if tb.port.Personal {
		plain, b, err := m.crypto.SplitConds(ctx, seal, a, q.Type, q.Where)
		if err != nil {
			return reasonr.Rows{}, err
		}
		where, blind = plain, b
	}
	query, ok, err := m.query(tb, a, q, where, blind, keys, cursor, s.db.Capabilities().OpaqueBytes)
	if err != nil {
		return reasonr.Rows{}, err
	}
	if !ok {
		query = &port.Query{Table: &tb.port, Limit: -1}
	}
	found, err := db.Find(ctx, tenant, query)
	if err != nil {
		return reasonr.Rows{}, err
	}
	rows, err := m.decodeRows(ctx, seal, tb, a, found)
	if err != nil {
		return reasonr.Rows{}, err
	}
	all := make([]storekit.Row, len(rows))
	for i := range rows {
		all[i] = rows[i].Row
	}
	if !ok {
		// Conditions on personal fields go over the decrypted values.
		matched := all[:0]
		for _, r := range all {
			if storekit.Matches(a, r.State, r.ID, a, q.Where) && (cursor == nil || storekit.AfterCursor(r, keys, a, cursor) > 0) {
				matched = append(matched, r)
			}
		}
		all = matched
		sort.SliceStable(all, func(i, j int) bool { return storekit.Compare(all[i], all[j], keys) < 0 })
	}
	return page(m, tb, a, q, keys, all)
}

// page builds a search result from the rows found in order: one row more
// than the page size means there is a next page.
func page(m *model, tb *table, a *reasonr.Arena, q *reasonr.FindSpec, keys []reasonr.SortKey, found []storekit.Row) (reasonr.Rows, error) {
	n := len(found)
	switch {
	case q.Page:
		n = min(n, q.Size)
	case q.Limit >= 0:
		n = min(n, q.Limit)
	}
	out := reasonr.Rows{Next: reasonr.None}
	if q.Page && n < len(found) && n > 0 {
		c, err := storekit.EncodeCursor(m.rec, tb.ti.Ref, found[n-1], keys)
		if err != nil {
			return reasonr.Rows{}, err
		}
		out.Next = a.String(c)
	}
	mk := a.Mark()
	for _, r := range found[:n] {
		mr := a.Mark()
		for _, f := range a.Elems(r.State) {
			a.Push(f)
		}
		a.Push(r.ID)
		a.Push(a.End(reasonr.KindRecord, tb.ti.Ref, mr))
	}
	out.Records = a.End(reasonr.KindList, 0, mk)
	return out, nil
}

// query is the search over key columns; ok == false — it is not
// expressible over columns (the rows are filtered here).
func (m *model) query(tb *table, a *reasonr.Arena, q *reasonr.FindSpec, where []reasonr.Cond, blind []storekit.BlindCond,
	keys []reasonr.SortKey, cursor []reasonr.Value, opaqueBytes bool) (*port.Query, bool, error) {
	if !tb.port.Ordered || opaqueBytes && tb.hasBytes() {
		return nil, false, nil
	}
	pq := &port.Query{Table: &tb.port, Limit: -1}
	for _, c := range where {
		col, kind, typ, ok := tb.col(c.Field)
		if !ok {
			return nil, false, nil
		}
		pc := port.Cond{Column: col, Op: port.CondOp(c.Op)}
		if c.Op == reasonr.OpIn {
			for _, x := range a.Elems(c.Value) {
				v, err := m.scalar(a, kind, typ, x)
				if err != nil {
					return nil, false, err
				}
				pc.Values = append(pc.Values, v)
			}
		} else {
			v, err := m.scalar(a, kind, typ, c.Value)
			if err != nil {
				return nil, false, err
			}
			pc.Values = []port.Scalar{v}
		}
		pq.Conds = append(pq.Conds, pc)
	}
	for _, bc := range blind {
		i, ok := tb.byPath[pathKey(bc.Field)]
		if !ok || tb.keys[i].Kind != port.ColBlind {
			return nil, false, nil
		}
		pc := port.Cond{Column: tb.keys[i].Name, Op: port.OpIn}
		if len(bc.Values) == 0 {
			pc.Op, pc.Values = port.OpEq, []port.Scalar{nil}
		}
		for _, v := range bc.Values {
			pc.Values = append(pc.Values, []byte(v))
		}
		pq.Conds = append(pq.Conds, pc)
	}
	type orderCol struct {
		col  string
		kind port.ColumnKind
		typ  reasonr.Type
	}
	var cols []orderCol
	for _, k := range keys {
		col, kind, typ, ok := tb.col(k.Field)
		if !ok {
			return nil, false, nil
		}
		cols = append(cols, orderCol{col, kind, typ})
		pq.Order = append(pq.Order, port.Order{Column: col, Desc: k.Desc})
	}
	cols = append(cols, orderCol{"id", tb.port.ID, reasonr.Type{Kind: reasonr.KindID, Ref: tb.ti.Ref}})
	pq.Order = append(pq.Order, port.Order{Column: "id"})
	if cursor != nil {
		for i, c := range cols {
			v, err := m.scalar(a, c.kind, c.typ, cursor[i])
			if err != nil {
				return nil, false, err
			}
			pq.After = append(pq.After, v)
		}
	}
	switch {
	case q.Page:
		pq.Limit = q.Size + 1
	case q.Limit >= 0:
		pq.Limit = q.Limit
	}
	return pq, true, nil
}

// hasBytes reports whether the table has a ColBytes key or id column.
func (t *table) hasBytes() bool {
	if t.port.ID == port.ColBytes {
		return true
	}
	for _, k := range t.port.Keys {
		if k.Kind == port.ColBytes {
			return true
		}
	}
	return false
}
