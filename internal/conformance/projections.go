package conformance

import (
	"context"
	"math/rand/v2"
	"testing"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// ProjectionFactory creates an empty asynchronous projection store.
type ProjectionFactory func(t *testing.T, catalog kind.Catalog) kind.ProjectionStore

// RunProjectionStore runs the asynchronous projection store set: writing a
// whole row by version, deletion marks, Applied, applying in a shuffled
// order with repeats, search, tenants (RFC §16).
func RunProjectionStore(t *testing.T, pf ProjectionFactory) {
	t.Run("Apply", func(t *testing.T) { testApply(t, pf) })
	t.Run("Shuffled", func(t *testing.T) { testShuffled(t, pf) })
	t.Run("Find", func(t *testing.T) { testProjectionFind(t, pf) })
}

// projections is the projection store fixture: the model schema and operations.
type projections struct {
	*fixture
	ps   kind.ProjectionStore
	pos  port.Position
	view reasonr.TypeRef
}

func newProjections(t *testing.T, pf ProjectionFactory) *projections {
	var ps kind.ProjectionStore
	f := newFixture(t, func(t *testing.T, c kind.Catalog) Stores {
		ps = pf(t, c)
		return Stores{Unit: nopUnits{}}
	}, options{})
	return &projections{fixture: f, ps: ps, view: f.typ("BoxView")}
}

// apply applies the operations of the next position.
func (p *projections) apply(ops ...reasonr.ProjectionOp) {
	p.t.Helper()
	p.pos++
	p.at(p.pos, ops...)
}

// at applies the operations of the position pos.
func (p *projections) at(pos port.Position, ops ...reasonr.ProjectionOp) {
	p.t.Helper()
	if err := p.ps.Apply(context.Background(), p.tenant, pos, p.a, ops, nil, kind.Forget{}); err != nil {
		p.t.Fatal(err)
	}
}

func (p *projections) upsert(box reasonr.Value, fields ...any) reasonr.ProjectionOp {
	op := reasonr.ProjectionOp{Kind: reasonr.ProjectionUpsert, Projection: p.view, ID: reasonr.ID(p.view, box)}
	for i := 0; i < len(fields); i += 2 {
		op.Set = append(op.Set, reasonr.FieldValue{Field: p.fieldRef("BoxView", fields[i].(string)), Value: fields[i+1].(reasonr.Value)})
	}
	return op
}

func (p *projections) remove(box reasonr.Value) reasonr.ProjectionOp {
	return reasonr.ProjectionOp{Kind: reasonr.ProjectionDelete, Projection: p.view, ID: reasonr.ID(p.view, box)}
}

func (p *projections) loadView(box reasonr.Value) (reasonr.Value, bool) {
	p.t.Helper()
	st, _, ok, err := p.ps.Load(context.Background(), p.tenant, p.a, p.view, reasonr.ID(p.view, box))
	if err != nil {
		p.t.Fatal(err)
	}
	return st.Value(), ok
}

// view is a BoxView row in Go: kind, size, owner.
type view struct {
	kind  string
	size  int64
	owner reasonr.Value
}

// expect checks the row box: want == nil — no row.
func (p *projections) expect(box reasonr.Value, want *view, what string) {
	p.t.Helper()
	st, ok := p.loadView(box)
	switch {
	case want == nil && ok:
		p.t.Fatalf("%s: there is a row: %s", what, p.a.Format(st))
	case want == nil:
	case !ok:
		p.t.Fatalf("%s: no row, want %+v", what, *want)
	case p.field(st, "BoxView", "kind") != p.kind(want.kind) || p.field(st, "BoxView", "size").Int() != want.size ||
		!reasonr.Equal(p.a, p.field(st, "BoxView", "owner"), p.a, want.owner):
		p.t.Fatalf("%s: row %s, want %+v", what, p.a.Format(st), *want)
	}
}

func (p *projections) applied() port.Position {
	p.t.Helper()
	pos, err := p.ps.Applied(context.Background(), p.tenant)
	if err != nil {
		p.t.Fatal(err)
	}
	return pos
}

func testApply(t *testing.T, pf ProjectionFactory) {
	p := newProjections(t, pf)
	b, k := p.id("Box"), p.id("Keeper")
	if p.applied() != 0 {
		t.Fatal("Applied of an empty store is not 0")
	}
	p.at(1, p.upsert(b, "kind", p.kind("big"), "size", reasonr.Int(5), "owner", k))
	p.expect(b, &view{"big", 5, k}, "upsert")
	p.at(2, p.upsert(b, "size", reasonr.Int(7)))
	p.expect(b, &view{"small", 7, reasonr.None}, "a row is written whole")
	// A repeated position and a late position change nothing.
	p.at(2, p.upsert(b, "size", reasonr.Int(9)))
	p.at(1, p.upsert(b, "size", reasonr.Int(8)))
	p.expect(b, &view{"small", 7, reasonr.None}, "a repeat and a late position")
	if p.applied() != 2 {
		t.Fatalf("Applied %d, want 2", p.applied())
	}
	p.at(3)
	if p.applied() != 3 {
		t.Fatalf("an empty position is not recorded: %d", p.applied())
	}
	// A deletion leaves a mark: a late write does not resurrect the row, a
	// later one does.
	p.at(5, p.remove(b))
	p.expect(b, nil, "deletion")
	p.at(4, p.upsert(b, "size", reasonr.Int(1)))
	p.expect(b, nil, "a write before the deletion")
	if p.applied() != 5 {
		t.Fatalf("Applied after a late position %d, want 5", p.applied())
	}
	p.at(6, p.upsert(b, "size", reasonr.Int(1)), p.remove(b), p.upsert(b, "size", reasonr.Int(2)))
	p.expect(b, &view{"small", 2, reasonr.None}, "a unit's result is its last operation")
	p.at(7, p.upsert(b, "size", reasonr.Int(3)), p.remove(b))
	p.expect(b, nil, "a deletion last in a unit")
	p.at(8, p.upsert(b, "kind", p.kind("big")))
	p.expect(b, &view{"big", 0, reasonr.None}, "a write after a deletion")
	p.tenant = T2
	p.expect(b, nil, "another tenant")
	if p.applied() != 0 {
		t.Fatal("the position is visible to another tenant")
	}
	p.at(1, p.upsert(b, "size", reasonr.Int(4)))
	p.expect(b, &view{"small", 4, reasonr.None}, "tenant versions are independent")
}

// testShuffled: a history of writes and deletions of several rows, applied
// in a shuffled order and with repeats, gives the same rows as in order.
func testShuffled(t *testing.T, pf ProjectionFactory) {
	p := newProjections(t, pf)
	owners := []reasonr.Value{reasonr.None, p.id("Keeper"), p.id("Keeper")}
	const units = 40
	for seed := uint64(1); seed <= 5; seed++ {
		rng := rand.New(rand.NewPCG(seed, 0))
		boxes := []reasonr.Value{p.id("Box"), p.id("Box"), p.id("Box"), p.id("Box")}
		want := make([]*view, len(boxes))
		history := make([][]reasonr.ProjectionOp, units)
		for i := range history {
			for range 1 + rng.IntN(3) {
				bi := rng.IntN(len(boxes))
				if rng.IntN(10) < 3 {
					history[i] = append(history[i], p.remove(boxes[bi]))
					want[bi] = nil
					continue
				}
				v := &view{kind: "small", owner: reasonr.None}
				var fields []any
				if rng.IntN(2) == 0 {
					v.kind = []string{"small", "big"}[rng.IntN(2)]
					fields = append(fields, "kind", p.kind(v.kind))
				}
				if rng.IntN(4) > 0 {
					v.size = rng.Int64N(100)
					fields = append(fields, "size", reasonr.Int(v.size))
				}
				if rng.IntN(2) == 0 {
					v.owner = owners[rng.IntN(len(owners))]
					fields = append(fields, "owner", v.owner)
				}
				history[i] = append(history[i], p.upsert(boxes[bi], fields...))
				want[bi] = v
			}
		}
		var order []int
		for i := range history {
			for range 1 + rng.IntN(2) {
				order = append(order, i)
			}
		}
		rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		for _, i := range order {
			p.at(port.Position(i+1), history[i]...)
		}
		for bi, b := range boxes {
			p.expect(b, want[bi], "a shuffled order")
		}
		if p.applied() != units {
			t.Fatalf("Applied %d, want %d", p.applied(), units)
		}
	}
}

func testProjectionFind(t *testing.T, pf ProjectionFactory) {
	p := newProjections(t, pf)
	k1 := p.id("Keeper")
	data := []struct {
		kind  string
		size  int64
		owner reasonr.Value
	}{{"big", 5, k1}, {"small", 3, reasonr.None}, {"small", 1, k1}, {"big", 2, reasonr.None}, {"small", 3, reasonr.None}, {"small", 9, k1}}
	var ids []reasonr.Value
	for _, d := range data {
		b := p.id("Box")
		ids = append(ids, b)
		p.apply(p.upsert(b, "kind", p.kind(d.kind), "size", reasonr.Int(d.size), "owner", d.owner))
	}
	checkFind(t, p.fixture, p.ps, p.view, ids, k1, func() {
		p.apply(p.upsert(p.id("Box"), "kind", p.kind("small"), "size", reasonr.Int(7)))
	})
}
