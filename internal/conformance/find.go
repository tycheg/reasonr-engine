package conformance

import (
	"context"
	"testing"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/kind"
)

// testFind: conditions of every kind, the index order and desc, limit,
// pages with a cursor, a search without an index and by identifier.
func testFind(t *testing.T, sf Factory) {
	f := newFixture(t, sf, options{})
	k1 := f.id("Keeper")
	data := []struct {
		kind  string
		size  int64
		owner reasonr.Value
	}{{"big", 5, k1}, {"small", 3, reasonr.None}, {"small", 1, k1}, {"big", 2, reasonr.None}, {"small", 3, reasonr.None}, {"small", 9, k1}}
	var ids []reasonr.Value
	for _, d := range data {
		b := f.id("Box")
		ids = append(ids, b)
		f.exec(admin, b, "Put", f.kind(d.kind), reasonr.Int(d.size), d.owner)
	}
	checkFind(t, f, f.st.Unit, f.typ("BoxView"), ids, k1, func() {
		f.exec(admin, f.id("Box"), "Put", f.kind("small"), reasonr.Int(7), reasonr.None)
	})
}

// checkFind checks a search over the BoxView rows created from the data of
// testFind; insert adds a row small/7 between pages.
func checkFind(t *testing.T, f *fixture, r kind.Reader, view reasonr.TypeRef, ids []reasonr.Value, k1 reasonr.Value, insert func()) {
	ctx := context.Background()
	kindF, sizeF, ownerF := f.fieldRef("BoxView", "kind"), f.fieldRef("BoxView", "size"), f.fieldRef("BoxView", "owner")
	sizes := func(q reasonr.FindSpec) ([]int64, reasonr.Rows) {
		t.Helper()
		if q.Cursor.Kind == reasonr.KindNone {
			q.Cursor = reasonr.None
		}
		q.Type = view
		rows, err := r.Find(ctx, f.tenant, f.a, &q)
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, r := range f.a.Elems(rows.Records) {
			e := f.a.Elems(r)
			if e[len(e)-1].Type != view {
				t.Fatalf("a row without an identifier: %s", f.a.Format(r))
			}
			out = append(out, e[sizeF].Int())
		}
		return out, rows
	}
	eq := func(got []int64, want ...int64) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	cond := func(field reasonr.FieldRef, op reasonr.CondOp, v reasonr.Value) reasonr.Cond {
		return reasonr.Cond{Field: reasonr.FieldPath{field}, Op: op, Value: v}
	}
	byKind, bySize := reasonr.IndexRef(0), reasonr.IndexRef(1)
	small, big := f.kind("small"), f.kind("big")
	for _, c := range []struct {
		name string
		q    reasonr.FindSpec
		want []int64
	}{
		{"eq and ge by index", reasonr.FindSpec{Index: byKind, Limit: 10, Where: []reasonr.Cond{
			cond(kindF, reasonr.OpEq, small), cond(sizeF, reasonr.OpGe, reasonr.Int(3))}}, []int64{3, 3, 9}},
		{"index order", reasonr.FindSpec{Index: byKind, Limit: 10}, []int64{1, 3, 3, 9, 2, 5}},
		{"desc", reasonr.FindSpec{Index: bySize, Limit: 10}, []int64{9, 5, 3, 3, 2, 1}},
		{"limit", reasonr.FindSpec{Index: bySize, Limit: 2}, []int64{9, 5}},
		{"ne", reasonr.FindSpec{Index: bySize, Limit: 10, Where: []reasonr.Cond{cond(kindF, reasonr.OpNe, small)}}, []int64{5, 2}},
		{"lt", reasonr.FindSpec{Index: bySize, Limit: 10, Where: []reasonr.Cond{cond(sizeF, reasonr.OpLt, reasonr.Int(3))}}, []int64{2, 1}},
		{"le", reasonr.FindSpec{Index: bySize, Limit: 10, Where: []reasonr.Cond{cond(sizeF, reasonr.OpLe, reasonr.Int(3))}}, []int64{3, 3, 2, 1}},
		{"gt", reasonr.FindSpec{Index: bySize, Limit: 10, Where: []reasonr.Cond{cond(sizeF, reasonr.OpGt, reasonr.Int(3))}}, []int64{9, 5}},
		{"in", reasonr.FindSpec{Index: bySize, Limit: 10, Where: []reasonr.Cond{
			cond(sizeF, reasonr.OpIn, f.a.List(reasonr.Int(1), reasonr.Int(5)))}}, []int64{5, 1}},
		{"eq none", reasonr.FindSpec{Index: bySize, Limit: 10, Where: []reasonr.Cond{cond(ownerF, reasonr.OpEq, reasonr.None)}}, []int64{3, 3, 2}},
		{"comparing with none is false", reasonr.FindSpec{Index: bySize, Limit: 10, Where: []reasonr.Cond{cond(ownerF, reasonr.OpGe, k1)}}, []int64{9, 5, 1}},
		{"query sort", reasonr.FindSpec{Index: byKind, Limit: 10, Where: []reasonr.Cond{cond(kindF, reasonr.OpEq, big)},
			Sort: []reasonr.SortKey{{Field: reasonr.FieldPath{sizeF}, Desc: true}}}, []int64{5, 2}},
		{"limit 0", reasonr.FindSpec{Index: byKind, Limit: 0}, nil},
	} {
		if got, _ := sizes(c.q); !eq(got, c.want...) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}

	// Without an index — all rows in id order.
	all, rows := sizes(reasonr.FindSpec{Index: reasonr.NoIndex, Limit: -1})
	if !eq(all, 5, 3, 1, 2, 3, 9) || rows.Next.Kind != reasonr.KindNone {
		t.Errorf("without an index: %v", all)
	}
	// By identifier.
	byID, _ := sizes(reasonr.FindSpec{Index: reasonr.NoIndex, Limit: -1, Where: []reasonr.Cond{
		{Field: reasonr.FieldPath{reasonr.IDField}, Op: reasonr.OpEq, Value: reasonr.ID(view, ids[2])}}})
	if !eq(byID, 1) {
		t.Errorf("by id: %v", byID)
	}

	// Pages of 4: the cursor after the last row, the end — Next none.
	var paged []int64
	cursor := reasonr.None
	for range 4 {
		got, rows := sizes(reasonr.FindSpec{Index: bySize, Page: true, Size: 4, Cursor: cursor})
		paged = append(paged, got...)
		if cursor = rows.Next; cursor.Kind == reasonr.KindNone {
			break
		}
	}
	if !eq(paged, 9, 5, 3, 3, 2, 1) {
		t.Errorf("pages: %v", paged)
	}
	// A row added before the cursor does not shift the next page.
	first, rows := sizes(reasonr.FindSpec{Index: bySize, Page: true, Size: 3})
	insert()
	second, _ := sizes(reasonr.FindSpec{Index: bySize, Page: true, Size: 3, Cursor: rows.Next})
	if !eq(first, 9, 5, 3) || !eq(second, 3, 2, 1) {
		t.Errorf("pages after an insert: %v %v", first, second)
	}
	if _, err := r.Find(ctx, f.tenant, f.a, &reasonr.FindSpec{Type: view, Index: bySize, Page: true, Size: 3, Cursor: f.a.String("@@")}); err == nil {
		t.Error("a wrong cursor: no error")
	}
}
