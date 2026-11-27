package porttest

import (
	"fmt"
	"testing"

	"github.com/tycheg/reasonr-engine/port"
)

// RunFind checks port.RowReader.Find: the conditions of every kind on key
// columns of every kind and on the id, the order with none first, pages
// after a row, the limit and the tenant. The store's Capabilities decide
// which conditions, descending keys and pages are checked.
func RunFind(t *testing.T, open Open) {
	s := laidOut(t, open).(port.RowStore)
	none := order(4, "", 0)
	none.Keys[0], none.Keys[1] = nil, nil
	commit(t, s, T1, unit(Orders, order(1, "ann", 10), order(2, "bob", 20), order(3, "ann", 30), none, order(5, "cid", 20)))
	commit(t, s, T2, unit(Orders, order(6, "ann", 5)))
	caps := s.Capabilities()
	byID := []port.Order{{Column: "id"}}
	cond := func(col string, op port.CondOp, vs ...port.Scalar) []port.Cond {
		return []port.Cond{{Column: col, Op: op, Values: vs}}
	}
	o := func(ns ...byte) []port.Scalar {
		out := make([]port.Scalar, len(ns))
		for i, n := range ns {
			out[i] = uuid(n)
		}
		return out
	}
	placed := func(n int64) string { return fmt.Sprint(Start.UnixNano() + n) }
	cases := []struct {
		name  string
		conds []port.Cond
		order []port.Order
		after []port.Scalar
		limit int
		want  []port.Scalar
		desc  bool // needs descending keys
		pages bool // needs pages
	}{
		{name: "eq", conds: cond("k_customer", port.OpEq, "ann"), want: o(1, 3)},
		{name: "eq none", conds: cond("k_customer", port.OpEq, nil), want: o(4)},
		{name: "ne", conds: cond("k_customer", port.OpNe, "ann"), want: o(2, 4, 5)},
		{name: "ne none", conds: cond("k_customer", port.OpNe, nil), want: o(1, 2, 3, 5)},
		{name: "gt", conds: cond("k_total", port.OpGt, int64(15)), want: o(2, 3, 5)},
		{name: "ge", conds: cond("k_total", port.OpGe, int64(20)), want: o(2, 3, 5)},
		{name: "lt", conds: cond("k_total", port.OpLt, int64(20)), want: o(1)},
		{name: "le", conds: cond("k_total", port.OpLe, int64(20)), want: o(1, 2, 5)},
		{name: "lt none", conds: cond("k_total", port.OpLt, nil), want: o()},
		{name: "in", conds: cond("k_customer", port.OpIn, "bob", "cid"), want: o(2, 5)},
		{name: "in none", conds: cond("k_customer", port.OpIn, "bob", nil), want: o(2, 4)},
		{name: "two conditions", conds: append(cond("k_customer", port.OpEq, "ann"), cond("k_total", port.OpGt, int64(10))...), want: o(3)},
		{name: "nanos", conds: cond("k_placed", port.OpGt, placed(3)), want: o(4, 5)},
		{name: "numeric", conds: cond("k_price", port.OpGe, "3.5"), want: o(3, 4, 5)},
		{name: "float", conds: cond("k_rate", port.OpLt, 0.6), want: o(1, 2)},
		{name: "bool", conds: cond("k_paid", port.OpEq, true), want: o(2, 4)},
		{name: "bytes", conds: cond("k_code", port.OpEq, []byte{'c', 3}), want: o(3)},
		{name: "opaque", conds: cond("k_route", port.OpEq, []byte{'r', 1}), want: o(1, 4)},
		{name: "id", conds: cond("id", port.OpEq, uuid(2)), want: o(2)},
		{name: "id in", conds: cond("id", port.OpIn, uuid(1), uuid(5), uuid(6)), want: o(1, 5)},
		{name: "order", order: []port.Order{{Column: "k_total"}, {Column: "id"}}, want: o(4, 1, 2, 5, 3)},
		{name: "order text", order: []port.Order{{Column: "k_customer"}, {Column: "id"}}, want: o(4, 1, 3, 2, 5)},
		{name: "order desc", order: []port.Order{{Column: "k_total", Desc: true}, {Column: "id"}}, want: o(3, 2, 5, 1, 4), desc: true},
		{name: "limit", order: []port.Order{{Column: "k_total"}, {Column: "id"}}, limit: 2, want: o(4, 1)},
		{name: "page", order: []port.Order{{Column: "k_total"}, {Column: "id"}}, after: []port.Scalar{int64(20), uuid(2)}, limit: 2,
			want: o(5, 3), pages: true},
		{name: "page after none", order: []port.Order{{Column: "k_total"}, {Column: "id"}}, after: []port.Scalar{nil, uuid(4)}, limit: 2,
			want: o(1, 2), pages: true},
		{name: "page desc", order: []port.Order{{Column: "k_total", Desc: true}, {Column: "id"}}, after: []port.Scalar{int64(20), uuid(2)},
			limit: 2, want: o(5, 1), desc: true, pages: true},
		{name: "page desc to none", order: []port.Order{{Column: "k_total", Desc: true}, {Column: "id"}}, after: []port.Scalar{int64(10), uuid(1)},
			limit: 5, want: o(4), desc: true, pages: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, cd := range c.conds {
				if !caps.Conds.Has(cd.Op) {
					t.Skipf("the store has no condition %d", cd.Op)
				}
			}
			if c.desc && !caps.SortDesc || c.pages && !caps.Pages {
				t.Skip("the store has no descending keys or pages")
			}
			q := &port.Query{Table: Orders, Conds: c.conds, Order: c.order, After: c.after, Limit: c.limit}
			if q.Order == nil {
				q.Order = byID
			}
			if q.Limit == 0 {
				q.Limit = -1
			}
			got, err := s.Find(ctx(t), T1, q)
			if err != nil || !sameIDs(got, c.want...) {
				t.Fatalf("Find: %x, %v; want %x", ids(got), err, c.want)
			}
		})
	}
	t.Run("rows", func(t *testing.T) {
		got, err := s.Find(ctx(t), T2, &port.Query{Table: Orders, Order: byID, Limit: -1})
		if err != nil || len(got) != 1 || got[0].Version != 1 || !sameJSON(got[0].State, state("order", 6)) {
			t.Fatalf("the rows of another tenant: %+v %v", got, err)
		}
	})
}
