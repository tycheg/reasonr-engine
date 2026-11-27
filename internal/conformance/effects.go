package conformance

import (
	"context"
	"testing"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// testOutbox: each committed unit of work with model effects gets an
// increasing position and goes to the outbox with events, versions and
// external requests; Read reads after a position and at most limit.
func testOutbox(t *testing.T, sf Factory) {
	f := newFixture(t, sf, options{})
	b := f.id("Box")
	var positions []port.Position
	for i := range 3 {
		res := f.command("", admin, b, "Put", f.kind("small"), reasonr.Int(int64(i)), reasonr.None)
		positions = append(positions, res.Position)
		res.Release()
	}
	if positions[0] == 0 || positions[1] <= positions[0] || positions[2] <= positions[1] {
		t.Fatalf("positions do not increase: %v", positions)
	}
	us := f.units(positions[0] - 1)
	if len(us) != 3 {
		t.Fatalf("%d units of work in the outbox", len(us))
	}
	for i, u := range us {
		if u.Position != positions[i] || u.Tenant != T1 || !u.At.Equal(Start) || len(u.Entities) != 1 {
			t.Fatalf("unit %d: position %d, tenant %q, %d entities", i, u.Position, u.Tenant, len(u.Entities))
		}
		ee := u.Entities[0]
		if ee.Type != b.Type || !reasonr.Equal(u.Arena, ee.ID, f.a, b) || ee.Version != uint64(i+1) || len(ee.Events) != 1 ||
			ee.Events[0].Name != f.method(b.Type, "stored") || u.Arena.Fields(ee.Events[0].Args)[1].Int() != int64(i) {
			t.Fatalf("unit %d: entity events", i)
		}
	}
	if us := f.units(positions[1]); len(us) != 1 || us[0].Position != positions[2] {
		t.Fatalf("Read after a position: %d", len(us))
	}
	limited, err := f.st.Effects.Read(context.Background(), 0, 2)
	if err != nil || len(limited) != 2 {
		t.Fatalf("Read with limit 2: %d %v", len(limited), err)
	}
	// A rejected command gets no position and does not go to the outbox.
	k := f.id("Keeper")
	if errs := f.exec(actor(k), k, "Join", f.a.String("Anne"), f.a.String("A")); errs != nil {
		t.Fatal(errs)
	}
	last := lastPosition(f)
	k2 := f.id("Keeper")
	res := f.command("", actor(k2), k2, "Join", f.a.String("Bob"), f.a.String("A"))
	if len(res.Errors) != 1 || res.Position != 0 {
		t.Fatalf("rejected: %d errors, position %d", len(res.Errors), res.Position)
	}
	res.Release()
	if n := len(f.units(last)); n != 0 {
		t.Fatalf("a rejected one in the outbox: %d", n)
	}
}

// testWait: Wait returns when a unit of work after the position appears,
// and when the context is cancelled.
func testWait(t *testing.T, sf Factory) {
	f := newFixture(t, sf, options{})
	b := f.id("Box")
	done := make(chan error, 1)
	go func() { done <- f.st.Effects.Wait(context.Background(), 0) }()
	select {
	case err := <-done:
		t.Fatalf("Wait returned before the commit: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	f.exec(admin, b, "Put", f.kind("small"), reasonr.Int(1), reasonr.None)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after the commit")
	}
	if err := f.st.Effects.Wait(context.Background(), 0); err != nil {
		t.Fatalf("Wait with an already committed one: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { done <- f.st.Effects.Wait(ctx, lastPosition(f)) }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Wait after cancel without an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after cancel")
	}
}

// testAsyncOps: operations on an asynchronous projection are not applied
// in the main store but go to the outbox.
func testAsyncOps(t *testing.T, sf Factory) {
	f := newFixture(t, sf, options{async: map[string]kind.ProjectionStore{"BoxView": nopProjections{}}})
	b := f.id("Box")
	f.exec(admin, b, "Put", f.kind("big"), reasonr.Int(4), reasonr.None)
	if _, _, ok := f.load(reasonr.ID(f.typ("BoxView"), b), "BoxView"); ok {
		t.Fatal("the asynchronous projection was written to the main store")
	}
	us := f.units(0)
	if len(us) != 1 || len(us[0].Projections) != 1 {
		t.Fatalf("operations in the outbox: %d units", len(us))
	}
	op := us[0].Projections[0]
	if op.Kind != reasonr.ProjectionUpsert || op.Projection != f.typ("BoxView") || !reasonr.Equal(us[0].Arena, op.ID, f.a, reasonr.ID(f.typ("BoxView"), b)) {
		t.Fatal("a wrong operation in the outbox")
	}
}

// nopProjections is an asynchronous store that keeps nothing.
type nopProjections struct{}

func (nopProjections) Load(context.Context, port.Tenant, *reasonr.Arena, reasonr.TypeRef, reasonr.Value) (reasonr.Record, uint64, bool, error) {
	return reasonr.Record{}, 0, false, nil
}

func (nopProjections) Find(_ context.Context, _ port.Tenant, a *reasonr.Arena, _ *reasonr.FindSpec) (reasonr.Rows, error) {
	return reasonr.Rows{Records: a.List(), Next: reasonr.None}, nil
}

func (nopProjections) Apply(context.Context, port.Tenant, port.Position, *reasonr.Arena, []reasonr.ProjectionOp, []port.KeyVersion, kind.Forget) error {
	return nil
}

func (nopProjections) Applied(context.Context, port.Tenant) (port.Position, error) { return 0, nil }
func (nopProjections) Capabilities() port.Capabilities                             { return port.Capabilities{} }

// nopUnits is a stub main store: the projection store checks commit no
// units of work.
type nopUnits struct{ nopProjections }

func (nopUnits) NextSequence(context.Context, port.Tenant, reasonr.TypeRef, int) (int64, error) {
	return 1, nil
}

func (nopUnits) Commit(context.Context, port.Tenant, *kind.Unit) (port.Position, error) {
	return 0, nil
}

func (nopUnits) Remembered(context.Context, port.Tenant, string, time.Time, *reasonr.Arena) (*kind.CommandResult, error) {
	return nil, nil
}
