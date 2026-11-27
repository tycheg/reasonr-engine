package conformance

import (
	"context"
	"errors"
	"testing"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// RunUnitStore runs the main store set: loading and versions, sequence, a
// conflict and a repeat, uniqueness, forget, migrations, search,
// idempotency, tenants, timers, the outbox and positions.
func RunUnitStore(t *testing.T, f Factory) {
	for _, c := range []struct {
		name string
		run  func(*testing.T, Factory)
	}{
		{"Load", testLoad}, {"Sequence", testSequence}, {"Conflict", testConflict},
		{"ConflictLimit", testConflictLimit}, {"Unique", testUnique}, {"Forget", testForget},
		{"Migrate", testMigrate}, {"Find", testFind}, {"Idempotency", testIdempotency},
		{"Tenants", testTenants}, {"Capabilities", testCapabilities},
		{"Timers", testTimers}, {"TimerLease", testTimerLease}, {"TimerOffset", testTimerOffset}, {"TimerGone", testTimerGone},
		{"TimerTenants", testTimerTenants},
		{"Outbox", testOutbox}, {"Wait", testWait}, {"AsyncOps", testAsyncOps}, {"EventFeed", testEventFeed},
		{"Features", testFeatures},
	} {
		t.Run(c.name, func(t *testing.T) { c.run(t, f) })
	}
}

func testLoad(t *testing.T, sf Factory) {
	f := newFixture(t, sf, options{})
	b := f.id("Box")
	st, v, ok := f.load(b, "Box")
	if ok || v != 0 || f.field(st, "Box", "size").Int() != 0 {
		t.Fatalf("a new entity: ok=%v v=%d", ok, v)
	}
	if errs := f.exec(admin, b, "Put", f.kind("big"), reasonr.Int(7), reasonr.None); errs != nil {
		t.Fatal(errs)
	}
	st, v, ok = f.load(b, "Box")
	if !ok || v != 1 || f.field(st, "Box", "size").Int() != 7 || f.field(st, "Box", "kind") != f.kind("big") {
		t.Fatalf("after Put: ok=%v v=%d %s", ok, v, f.a.Format(st))
	}
	view, _, ok := f.load(reasonr.ID(f.typ("BoxView"), b), "BoxView")
	if !ok || f.field(view, "BoxView", "size").Int() != 7 {
		t.Fatalf("projection: ok=%v %s", ok, f.a.Format(view))
	}
	f.exec(admin, b, "Put", f.kind("small"), reasonr.Int(8), f.id("Keeper"))
	if _, v, _ = f.load(b, "Box"); v != 2 {
		t.Fatalf("version %d", v)
	}
	// A synchronous projection row is written whole: owner not passed is none.
	if errs := f.exec(admin, b, "Relabel", f.kind("big")); errs != nil {
		t.Fatal(errs)
	}
	view, _, ok = f.load(reasonr.ID(f.typ("BoxView"), b), "BoxView")
	if !ok || f.field(view, "BoxView", "kind") != f.kind("big") || f.field(view, "BoxView", "size").Int() != 8 ||
		f.field(view, "BoxView", "owner").Kind != reasonr.KindNone {
		t.Fatalf("the projection after Relabel: ok=%v %s", ok, f.a.Format(view))
	}
}

func testSequence(t *testing.T, sf Factory) {
	f := newFixture(t, sf, options{})
	s1, s2 := f.id("Ticket"), f.id("Ticket")
	if s1.Int() != 1 || s2.Int() != 2 {
		t.Errorf("sequence: %d %d", s1.Int(), s2.Int())
	}
	first, err := f.st.Unit.NextSequence(context.Background(), T1, f.typ("Ticket"), 3)
	if err != nil || first != 3 {
		t.Errorf("NextSequence(3) = %d, %v; want 3", first, err)
	}
	if s := f.id("Ticket"); s.Int() != 6 {
		t.Errorf("after allocating three: %d", s.Int())
	}
	if errs := f.exec(admin, s1, "Issue"); errs != nil {
		t.Fatal(errs)
	}
	if _, v, ok := f.load(s1, "Ticket"); !ok || v != 1 {
		t.Errorf("Ticket: ok=%v v=%d", ok, v)
	}
}

// testConflict: a version conflict at commit — the command repeats on the
// new state.
func testConflict(t *testing.T, sf Factory) {
	f := newFixture(t, sf, options{})
	b := f.id("Box")
	f.exec(admin, b, "Put", f.kind("small"), reasonr.Int(1), reasonr.None)
	hooks := 0
	f.hook = func(*reasonr.UnitOfWork) {
		if hooks > 0 {
			return
		}
		hooks++
		f.exec(reasonr.Caller{Kind: reasonr.CallerTimer}, b, "touch") // another command gets in first
	}
	f.exec(reasonr.Caller{Kind: reasonr.CallerTimer}, b, "touch")
	st, v, _ := f.load(b, "Box")
	if hooks != 1 || v != 3 || f.field(st, "Box", "touches").Int() != 2 {
		t.Fatalf("hooks=%d version %d touches %d", hooks, v, f.field(st, "Box", "touches").Int())
	}
}

func testConflictLimit(t *testing.T, sf Factory) {
	f := newFixture(t, sf, options{})
	b := f.id("Box")
	f.exec(admin, b, "Put", f.kind("small"), reasonr.Int(1), reasonr.None)
	inner := false
	f.hook = func(*reasonr.UnitOfWork) {
		if inner {
			return
		}
		inner = true
		f.exec(reasonr.Caller{Kind: reasonr.CallerTimer}, b, "touch")
		inner = false
	}
	_, err := f.k.Command(context.Background(), &kind.CommandRequest{Tenant: T1, Call: reasonr.CommandCall{
		Entity: b.Type, ID: b, Command: f.method(b.Type, "touch"), Args: f.a.Record(0), Arena: f.a,
		Caller: reasonr.Caller{Kind: reasonr.CallerTimer},
	}})
	if !errors.Is(err, port.ErrConflict) {
		t.Fatalf("expected ErrConflict: %v", err)
	}
}

func testUnique(t *testing.T, sf Factory) {
	f := newFixture(t, sf, options{})
	k1, k2, k3 := f.id("Keeper"), f.id("Keeper"), f.id("Keeper")
	code := f.a.String("A1")
	if errs := f.exec(actor(k1), k1, "Join", f.a.String("Anne"), code); errs != nil {
		t.Fatal(errs)
	}
	errs := f.exec(actor(k2), k2, "Join", f.a.String("Bob"), code)
	if len(errs) != 1 || errs[0] != `codeTakenT0("A1")` {
		t.Fatalf("errors: %v", errs)
	}
	if _, _, ok := f.load(k2, "Keeper"); ok {
		t.Fatal("the effects of a rejected unit of work are applied")
	}
	// none does not take part in uniqueness.
	if errs := f.exec(actor(k2), k2, "Join", f.a.String("Bob"), reasonr.None); errs != nil {
		t.Fatal(errs)
	}
	if errs := f.exec(actor(k3), k3, "Join", f.a.String("Vera"), reasonr.None); errs != nil {
		t.Fatal(errs)
	}
	// Writing one's own value again is not a violation.
	if errs := f.exec(actor(k1), k1, "Join", f.a.String("Anne"), code); errs != nil {
		t.Fatal(errs)
	}
	// A value is freed when its owner changes the code.
	if errs := f.exec(actor(k1), k1, "Join", f.a.String("Anne"), f.a.String("A2")); errs != nil {
		t.Fatal(errs)
	}
	if errs := f.exec(actor(k2), k2, "Join", f.a.String("Bob"), code); errs != nil {
		t.Fatalf("a freed value: %v", errs)
	}
}

func testForget(t *testing.T, sf Factory) {
	f := newFixture(t, sf, options{})
	k1, k2 := f.id("Keeper"), f.id("Keeper")
	f.exec(actor(k1), k1, "Join", f.a.String("Anne"), reasonr.None)
	f.exec(actor(k2), k2, "Join", f.a.String("Bob"), reasonr.None)
	before := lastPosition(f)
	f.exec(actor(k1), k1, "Leave")
	st, _, ok := f.load(k1, "Keeper")
	if name := f.field(st, "Keeper", "name"); !ok || name.Kind != reasonr.KindNone {
		t.Fatalf("the name of the forgotten one: %s", f.a.Format(name))
	}
	if st, _, _ := f.load(k2, "Keeper"); f.a.Text(f.field(st, "Keeper", "name")) != "Bob" {
		t.Fatal("the data of another subject is erased")
	}
	us := f.units(before)
	if len(us) != 1 || len(us[0].Forget) != 1 || !reasonr.Equal(us[0].Arena, us[0].Forget[0].ID, f.a, k1) {
		t.Fatalf("forget in the outbox: %d units", len(us))
	}
}

func testMigrate(t *testing.T, sf Factory) {
	f := newFixture(t, sf, options{})
	var boxes []reasonr.Value
	for range 3 {
		b := f.id("Box")
		boxes = append(boxes, b)
		f.exec(admin, b, "Put", f.kind("small"), reasonr.Int(1), reasonr.None)
	}
	before := lastPosition(f)
	if err := f.k.Migrate(context.Background(), T1, "retouch"); err != nil {
		t.Fatal(err)
	}
	if n := len(f.units(before)); n != 3 {
		t.Errorf("%d units of work, want 3", n)
	}
	for _, b := range boxes {
		if st, _, _ := f.load(b, "Box"); f.field(st, "Box", "touches").Int() != 1 {
			t.Errorf("%s is not processed", f.a.Format(b))
		}
	}
	if err := f.k.Migrate(context.Background(), T1, "nope"); err == nil {
		t.Error("an unknown migration: no error")
	}
}

// testIdempotency: a repeated command with the same key gets the same
// response and does not run again; tenant keys are independent.
func testIdempotency(t *testing.T, sf Factory) {
	f := newFixture(t, sf, options{})
	k1, k2 := f.id("Keeper"), f.id("Keeper")
	f.exec(actor(k1), k1, "Join", f.a.String("Anne"), f.a.String("A1"))
	f.exec(actor(k2), k2, "Join", f.a.String("Bob"), reasonr.None)

	first := f.command("poke-1", actor(k1), k1, "Poke", k2)
	pos, notes := first.Position, len(first.Notifications)
	first.Release()
	if pos == 0 || notes != 1 {
		t.Fatalf("an accepted command: position %d, %d notifications", pos, notes)
	}
	again := f.command("poke-1", actor(k1), k1, "Poke", k2)
	if !again.Replayed || again.Position != pos || len(again.Notifications) != 1 || again.Notifications[0].Name != f.method(k1.Type, "pokeSent") {
		t.Fatalf("repeat: replayed=%v position %d, %d notifications", again.Replayed, again.Position, len(again.Notifications))
	}
	if !reasonr.Equal(again.Arena, again.Notifications[0].To, f.a, k1) {
		t.Fatal("repeat: the notification is not to the initiator")
	}
	again.Release()
	if n := len(f.units(pos)); n != 0 {
		t.Fatalf("the repeat ran the command: %d new units of work", n)
	}

	// A rejected command answers by key too.
	rejected := f.command("join-2", actor(k2), k2, "Join", f.a.String("Bob"), f.a.String("A1"))
	errs := errorTexts(rejected)
	rejected.Release()
	if len(errs) != 1 {
		t.Fatalf("expected a uniqueness error: %v", errs)
	}
	f.exec(actor(k1), k1, "Join", f.a.String("Anne"), f.a.String("A3")) // the code is free
	replay := f.command("join-2", actor(k2), k2, "Join", f.a.String("Bob"), f.a.String("A1"))
	if got := errorTexts(replay); !replay.Replayed || len(got) != 1 || got[0] != errs[0] {
		t.Fatalf("a repeat of the rejected one: replayed=%v %v", replay.Replayed, got)
	}
	replay.Release()

	// The same key in another tenant is another call.
	f.tenant = T2
	k3 := f.id("Keeper")
	other := f.command("join-2", actor(k3), k3, "Join", f.a.String("Vera"), f.a.String("A1"))
	if other.Replayed || len(other.Errors) != 0 {
		t.Fatalf("the key of another tenant: replayed=%v %v", other.Replayed, errorTexts(other))
	}
	other.Release()
}

// testTenants: tenant data and counters are isolated.
func testTenants(t *testing.T, sf Factory) {
	f := newFixture(t, sf, options{})
	b := f.id("Box")
	f.exec(admin, b, "Put", f.kind("big"), reasonr.Int(1), reasonr.None)
	s1 := f.id("Ticket")
	f.tenant = T2
	if _, _, ok := f.load(b, "Box"); ok {
		t.Fatal("an entity is visible to another tenant")
	}
	if s := f.id("Ticket"); s.Int() != 1 || s1.Int() != 1 {
		t.Fatalf("tenant sequences: %d %d", s1.Int(), s.Int())
	}
	f.exec(admin, b, "Put", f.kind("small"), reasonr.Int(2), reasonr.None)
	st, v, _ := f.load(b, "Box")
	if v != 1 || f.field(st, "Box", "size").Int() != 2 {
		t.Fatalf("the same id in T2: version %d", v)
	}
	f.tenant = T1
	if st, v, _ := f.load(b, "Box"); v != 1 || f.field(st, "Box", "size").Int() != 1 {
		t.Fatalf("T1 changed by a T2 command: version %d", v)
	}
	rows, err := f.st.Unit.Find(context.Background(), T2, f.a, &reasonr.FindSpec{Type: f.typ("BoxView"), Index: 0, Limit: 10, Cursor: reasonr.None})
	if err != nil || f.a.Len(rows.Records) != 1 {
		t.Fatalf("search in T2: %v", err)
	}
}

func testCapabilities(t *testing.T, sf Factory) {
	f := newFixture(t, sf, options{})
	c := f.st.Unit.Capabilities()
	if !c.Transactions || !c.Unique || !c.Conds.Has(port.OpEq) || !c.Pages {
		t.Fatalf("the main store needs transactions, uniqueness, equality and pages: %+v", c)
	}
}

// lastPosition returns the position of the last unit of work in the outbox.
func lastPosition(f *fixture) port.Position {
	us := f.units(0)
	if len(us) == 0 {
		return 0
	}
	return us[len(us)-1].Position
}
