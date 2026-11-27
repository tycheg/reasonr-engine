package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/conformance"
	"github.com/tycheg/reasonr-engine/kind"
)

// Tests of the in-memory engine: the event store, identifiers by Seed,
// Advance, the delivery journal, the determinism check. The conformance
// tests (conformance_test.go) check the store behavior. The model is engine/conformance.

type fixture struct {
	t *testing.T
	m *reasonr.Model
	s *reasonr.Schema
	e *Engine
	a *reasonr.Arena // the call arena
}

func newFixture(t *testing.T, opts Options) *fixture {
	m := conformance.Machine(t)
	return &fixture{t: t, m: m, s: m.Schema(), e: New(m, opts), a: reasonr.NewArena()}
}

func (f *fixture) typ(name string) reasonr.TypeRef {
	f.t.Helper()
	for _, ti := range f.s.Types {
		if ti.Name == name {
			return ti.Ref
		}
	}
	f.t.Fatalf("no type %s", name)
	return 0
}

func (f *fixture) method(t reasonr.TypeRef, name string) reasonr.MethodRef {
	f.t.Helper()
	for _, mi := range f.s.Type(t).Methods {
		if mi.Name == name {
			return mi.Ref
		}
	}
	f.t.Fatalf("no method %s", name)
	return 0
}

func (f *fixture) id(t string) reasonr.Value {
	f.t.Helper()
	v, err := f.e.NewID(context.Background(), f.a, f.typ(t))
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

var admin = reasonr.Caller{Kind: reasonr.CallerUser, Roles: []string{"admin"}}

func actor(id reasonr.Value) reasonr.Caller {
	return reasonr.Caller{Kind: reasonr.CallerUser, Subject: id, Actor: id.Type}
}

// exec runs a command and returns the errors (names); a fault fails the test.
func (f *fixture) exec(c reasonr.Caller, id reasonr.Value, cmd string, args ...reasonr.Value) []string {
	f.t.Helper()
	out, err := f.e.Execute(context.Background(), reasonr.CommandCall{
		Entity: id.Type, ID: id, Command: f.method(id.Type, cmd), Args: f.a.Record(0, args...), Arena: f.a, Caller: c,
	})
	if err != nil {
		f.t.Fatalf("%s: %v", cmd, err)
	}
	defer out.Release()
	var names []string
	for _, be := range out.Errors {
		names = append(names, be.Name+out.Arena.Format(be.Args.Value()))
	}
	return names
}

// load returns a record's state (in the arena f.a), version and existence.
func (f *fixture) load(id reasonr.Value, t string) (reasonr.Value, uint64, bool) {
	f.t.Helper()
	st, v, ok, err := f.e.Load(context.Background(), f.a, f.typ(t), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return st.Value(), v, ok
}

func (f *fixture) field(rec reasonr.Value, t, name string) reasonr.Value {
	f.t.Helper()
	for i, fi := range f.s.Type(f.typ(t)).Fields {
		if fi.Name == name {
			return f.a.Elems(rec)[i]
		}
	}
	f.t.Fatalf("no field %s.%s", t, name)
	return reasonr.None
}

func (f *fixture) kind(name string) reasonr.Value {
	ti := f.s.Type(f.typ("Kind"))
	for i, n := range ti.Enum {
		if n == name {
			return reasonr.Enum(ti.Ref, i)
		}
	}
	f.t.Fatalf("no value %s", name)
	return reasonr.None
}

func TestLoad(t *testing.T) {
	f := newFixture(t, Options{})
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
	f.exec(admin, b, "Put", f.kind("small"), reasonr.Int(8), reasonr.None)
	if _, v, _ = f.load(b, "Box"); v != 2 {
		t.Fatalf("version %d", v)
	}
	evs, err := f.e.Events(f.a, f.typ("Box"), b)
	if err != nil || len(evs) != 2 || f.a.Fields(evs[1].Args)[1].Int() != 8 {
		t.Fatalf("event store: %v %v", evs, err)
	}
}

func TestIDs(t *testing.T) {
	f := newFixture(t, Options{Seed: 0xabcdef})
	u := f.id("Box")
	want := [16]byte{5: 0xab, 6: 0x4d, 7: 0xef, 8: 0x80, 15: 1} // 00000000-00ab-4def-8000-000000000001
	if u.UUID() != want {
		t.Errorf("uuid: %x", u.UUID())
	}
	s1, s2 := f.id("Ticket"), f.id("Ticket")
	if s1.Lo != 1 || s2.Lo != 2 {
		t.Errorf("sequence: %d %d", s1.Lo, s2.Lo)
	}
	if _, err := f.e.NewID(context.Background(), f.a, f.typ("BoxView")); err == nil {
		t.Error("NewID of a derived identifier: no error")
	}
	g := newFixture(t, Options{Seed: 0xabcdef})
	if u2 := g.id("Box"); u2 != u {
		t.Error("identifiers with one Seed differ")
	}
}

func TestForget(t *testing.T) {
	f := newFixture(t, Options{})
	k1, k2 := f.id("Keeper"), f.id("Keeper")
	f.exec(actor(k1), k1, "Join", f.a.String("Anne"), reasonr.None)
	f.exec(actor(k2), k2, "Join", f.a.String("Bob"), reasonr.None)
	f.exec(actor(k1), k1, "Leave")
	st, v, _ := f.load(k1, "Keeper")
	if name := f.field(st, "Keeper", "name"); name.Kind != reasonr.KindNone || v != 3 {
		t.Fatalf("the name of the forgotten: %s, version %d", f.a.Format(name), v)
	}
	evs, err := f.e.Events(f.a, f.typ("Keeper"), k1)
	if err != nil || f.a.Fields(evs[0].Args)[0].Kind != reasonr.KindNone {
		t.Fatalf("the event argument is not erased: %v", err)
	}
	if st, _, _ := f.load(k2, "Keeper"); f.a.Text(f.field(st, "Keeper", "name")) != "Bob" {
		t.Fatal("the data of another subject is erased")
	}
}

func TestTimers(t *testing.T) {
	f := newFixture(t, Options{})
	ctx := context.Background()
	b1, b2 := f.id("Box"), f.id("Box")
	for _, b := range []reasonr.Value{b1, b2} {
		f.exec(admin, b, "Put", f.kind("small"), reasonr.Int(1), reasonr.None)
	}
	f.exec(admin, b2, "Arm", reasonr.Duration(30*time.Second))
	f.exec(admin, b1, "Arm", reasonr.Duration(time.Minute))
	if f.e.Timers() != 2 {
		t.Fatalf("%d timers", f.e.Timers())
	}
	var fired []reasonr.Value
	each := func(res *kind.CommandResult) {
		if w := res.Outcome.Work; w != nil {
			fired = append(fired, f.a.Copy(res.Arena, w.Entities[0].ID))
		}
	}
	start := f.e.Now()
	if err := f.e.Advance(ctx, 29*time.Second, each); err != nil || len(fired) != 0 {
		t.Fatalf("fired too early: %v %v", fired, err)
	}
	if err := f.e.Advance(ctx, 31*time.Second, each); err != nil {
		t.Fatal(err)
	}
	if len(fired) != 2 || fired[0] != b2 || fired[1] != b1 || !f.e.Now().Equal(start.Add(time.Minute)) {
		t.Fatalf("fired %v, clock %v", fired, f.e.Now())
	}
	if _, _, ok := f.load(reasonr.ID(f.typ("BoxView"), b1), "BoxView"); ok {
		t.Fatal("the timer did not delete the projection row")
	}
	st, _, _ := f.load(b1, "Box")
	if f.field(st, "Box", "timer").Kind != reasonr.KindNone {
		t.Fatal("the timer is not reset")
	}
	_, sent := f.e.Deliveries(0)
	if len(sent) != 2 || sent[0].Request == nil {
		t.Fatalf("delivery journal: %d", len(sent))
	}
	// A deleted timer does not fire.
	f.exec(admin, b1, "Arm", reasonr.Duration(time.Second))
	f.exec(admin, b1, "Disarm")
	fired = nil
	if err := f.e.Advance(ctx, time.Hour, each); err != nil || len(fired) != 0 || f.e.Timers() != 0 {
		t.Fatalf("a deleted timer: %v %v", fired, err)
	}
}

// TestDeliveries: the delivery journal — notifications to the initiator
// (from the response), to other actors (through the hub) and external
// requests (from the outbox).
func TestDeliveries(t *testing.T) {
	f := newFixture(t, Options{})
	k1, k2 := f.id("Keeper"), f.id("Keeper")
	f.exec(actor(k1), k1, "Join", f.a.String("Anne"), reasonr.None)
	f.exec(actor(k2), k2, "Join", f.a.String("Bob"), reasonr.None)
	sub, err := f.e.Kernel().Subscribe(context.Background(), Tenant, f.a, k2)
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	res, err := f.e.Execute(context.Background(), reasonr.CommandCall{
		Entity: k1.Type, ID: k1, Command: f.method(k1.Type, "Poke"), Args: f.a.Record(0, k2), Arena: f.a, Caller: actor(k1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Notifications) != 1 || res.Notifications[0].Name != f.method(k1.Type, "pokeSent") {
		t.Fatalf("notifications to the initiator in the response: %d", len(res.Notifications))
	}
	res.Release()
	select {
	case d := <-sub.C():
		if d.Notification.Name != f.method(k1.Type, "poked") {
			t.Fatalf("the subscriber got %d", d.Notification.Name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the notification to another actor was not delivered")
	}
	log, sent := f.e.Deliveries(0)
	if len(sent) != 2 || sent[0].Notification == nil || sent[1].Notification == nil ||
		!log.Equal(sent[0].Notification.To, log.Copy(f.a, k1)) || !log.Equal(sent[1].Notification.To, log.Copy(f.a, k2)) {
		t.Fatalf("delivery journal: %d entries", len(sent))
	}
}

func TestDeterminism(t *testing.T) {
	f := newFixture(t, Options{CheckDeterminism: true})
	b := f.id("Box")
	if errs := f.exec(admin, b, "Put", f.kind("big"), reasonr.Int(3), reasonr.None); errs != nil {
		t.Fatal(errs)
	}
	f.exec(admin, b, "Arm", reasonr.Duration(time.Minute))
	if f.e.Timers() != 1 {
		t.Fatal("the timer is not scheduled")
	}
}

// flaky is an interpreter whose second execution differs from the first.
type flaky struct {
	reasonr.Interpreter
	calls int
	extra func(ctx context.Context, env reasonr.Env, out *reasonr.Outcome)
}

func (f *flaky) Execute(ctx context.Context, call reasonr.CommandCall, env reasonr.Env) (*reasonr.Outcome, error) {
	out, err := f.Interpreter.Execute(ctx, call, env)
	f.calls++
	if err == nil && f.calls%2 == 0 {
		f.extra(ctx, env, out)
	}
	return out, err
}

func TestNondeterminism(t *testing.T) {
	for name, extra := range map[string]func(context.Context, reasonr.Env, *reasonr.Outcome){
		"identifiers": func(ctx context.Context, env reasonr.Env, out *reasonr.Outcome) {
			_, _ = env.NewID(ctx, out.Arena, 0)
		},
		"unit of work": func(_ context.Context, _ reasonr.Env, out *reasonr.Outcome) {
			out.Work.Entities[0].Version++
		},
	} {
		f := newFixture(t, Options{})
		f.e = New(&flaky{Interpreter: f.m, extra: extra}, Options{CheckDeterminism: true})
		b := f.id("Box")
		_, err := f.e.Execute(context.Background(), reasonr.CommandCall{
			Entity: b.Type, ID: b, Command: f.method(b.Type, "Put"), Arena: f.a, Caller: admin,
			Args: f.a.Record(0, f.kind("big"), reasonr.Int(1), reasonr.None),
		})
		if !errors.Is(err, ErrNondetermined) {
			t.Errorf("%s: expected ErrNondetermined: %v", name, err)
		}
	}
}
