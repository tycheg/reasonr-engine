package kernel_test

import (
	"context"
	"errors"
	"testing"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/memory"
	"github.com/tycheg/reasonr-engine/internal/conformance"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Kernel tests on memory adapters: asynchronous projections and after,
// notifications to the initiator and to other actors, timers with a
// business error and a fault, an idempotency key race.

const tenant = port.DefaultTenant

var start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// faulty is an interpreter whose timer commands are rejected (reject) or
// fail (fault).
type faulty struct {
	reasonr.Interpreter
	reject, fault bool
}

var errFault = errors.New("fault")

func (f *faulty) Execute(ctx context.Context, call reasonr.CommandCall, env reasonr.Env) (*reasonr.Outcome, error) {
	if call.Caller.Kind == reasonr.CallerTimer {
		switch {
		case f.fault:
			return nil, errFault
		case f.reject:
			out := reasonr.NewOutcome(reasonr.GetArena())
			out.SetErrors([]reasonr.BusinessError{{Name: "cannotExpire", Args: out.Arena.Record(0)}})
			return out, nil
		}
	}
	return f.Interpreter.Execute(ctx, call, env)
}

type fixture struct {
	t        *testing.T
	interp   *faulty
	s        *reasonr.Schema
	programs *kernel.Programs
	store    *memory.Store
	views    *memory.Store // the store of the asynchronous BoxView
	hub      *memory.Hub
	clock    *memory.Clock
	k        *kernel.Kernel
	a        *reasonr.Arena
}

// newFixture is a kernel on memory adapters; with async the BoxView
// projection is stored asynchronously in f.views.
func newFixture(t *testing.T, cfg kernel.Config, async bool) *fixture {
	m := conformance.Machine(t)
	f := &fixture{t: t, interp: &faulty{Interpreter: m}, s: m.Schema(), programs: kernel.NewPrograms(),
		hub: memory.NewHub(false), clock: memory.NewClock(start), a: reasonr.NewArena()}
	f.programs.Set(tenant, f.interp)
	f.store, f.views = memory.NewStore(f.programs), memory.NewStore(f.programs)
	f.k = f.kernel(cfg, async)
	return f
}

// kernel is a kernel on the fixture's stores.
func (f *fixture) kernel(cfg kernel.Config, async bool) *kernel.Kernel {
	cfg.Programs, cfg.Store, cfg.Timers, cfg.Hub, cfg.Clock = f.programs, f.store, f.store, f.hub, f.clock
	if async {
		cfg.Async = map[string]kind.ProjectionStore{"BoxView": f.views}
	}
	k, err := kernel.New(cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	return k
}
func (f *fixture) typ(name string) reasonr.TypeRef { return f.s.Lookup(name).Ref }

func (f *fixture) method(t reasonr.TypeRef, name string) reasonr.MethodRef {
	return f.s.Type(t).Method(name).Ref
}

func (f *fixture) id(t string) reasonr.Value {
	f.t.Helper()
	v, err := f.k.NewID(context.Background(), tenant, f.typ(t))
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

var admin = reasonr.Caller{Kind: reasonr.CallerUser, Roles: []string{"admin"}}

func actor(id reasonr.Value) reasonr.Caller {
	return reasonr.Caller{Kind: reasonr.CallerUser, Subject: id, Actor: id.Type}
}

func (f *fixture) command(key string, c reasonr.Caller, id reasonr.Value, cmd string, args ...reasonr.Value) *kind.CommandResult {
	f.t.Helper()
	res, err := f.k.Command(context.Background(), &kind.CommandRequest{Tenant: tenant, Idempotency: key, Call: reasonr.CommandCall{
		Entity: id.Type, ID: id, Command: f.method(id.Type, cmd), Args: f.a.Record(0, args...), Arena: f.a, Caller: c,
	}})
	if err != nil {
		f.t.Fatalf("%s: %v", cmd, err)
	}
	return res
}

func (f *fixture) put(b reasonr.Value, size int64) port.Position {
	f.t.Helper()
	res := f.command("", admin, b, "Put", reasonr.Enum(f.typ("Kind"), 0), reasonr.Int(size), reasonr.None)
	defer res.Release()
	if len(res.Errors) > 0 {
		f.t.Fatalf("Put: %s", res.Errors[0].Name)
	}
	return res.Position
}

// size calls the BoxView.Size query of the kernel k with after.
func (f *fixture) size(k *kernel.Kernel, b reasonr.Value, after port.Position) (int64, string, error) {
	view := f.typ("BoxView")
	out, err := k.Query(context.Background(), &kind.QueryRequest{Tenant: tenant, After: after, Call: reasonr.QueryCall{
		Type: view, Query: f.method(view, "Size"), Args: f.a.Record(0, b), Arena: f.a, Caller: admin,
	}})
	if err != nil {
		return 0, "", err
	}
	defer out.Release()
	if len(out.Errors) > 0 {
		return 0, out.Errors[0].Name, nil
	}
	return out.Arena.Elems(out.Result)[0].Int(), "", nil
}

// TestAsyncAfter: an asynchronous projection fills from the outbox; a
// query with after waits for it and gets ErrNotReady if it does not catch
// up; without after it reads at once.
func TestAsyncAfter(t *testing.T) {
	f := newFixture(t, kernel.Config{ConsistencyWait: 50 * time.Millisecond}, true)
	b := f.id("Box")
	pos := f.put(b, 7)
	if _, _, ok, _ := f.store.Load(context.Background(), tenant, f.a, f.typ("BoxView"), reasonr.ID(f.typ("BoxView"), b)); ok {
		t.Fatal("the asynchronous projection was written to the main store")
	}
	if _, name, err := f.size(f.k, b, 0); err != nil || name != "noBox" {
		t.Fatalf("without after before delivery: %q %v", name, err)
	}
	if _, _, err := f.size(f.k, b, pos); !errors.Is(err, port.ErrNotReady) {
		t.Fatalf("after before delivery: %v", err)
	}
	var read port.Position
	pump := func() {
		units, err := f.store.Read(context.Background(), read, 0)
		if err != nil {
			t.Error(err)
			return
		}
		for _, u := range units {
			read = u.Position
			if err := f.views.Apply(context.Background(), u.Tenant, u.Position, u.Arena, u.Projections, u.Keys, kind.Forget{}); err != nil {
				t.Error(err)
			}
		}
	}
	pump()
	if size, name, err := f.size(f.k, b, pos); err != nil || name != "" || size != 7 {
		t.Fatalf("after after delivery: %d %q %v", size, name, err)
	}
	// The query waits for a delivery that happens during the wait.
	pos = f.put(b, 9)
	patient := f.kernel(kernel.Config{ConsistencyWait: 5 * time.Second}, true)
	go func() {
		time.Sleep(20 * time.Millisecond)
		pump()
	}()
	if size, name, err := f.size(patient, b, pos); err != nil || name != "" || size != 9 {
		t.Fatalf("after during delivery: %d %q %v", size, name, err)
	}
}

// TestNotifications: notifications to the initiator are in the response,
// to another actor in the hub; the initiator does not get them by subscription.
func TestNotifications(t *testing.T) {
	f := newFixture(t, kernel.Config{}, false)
	k1, k2 := f.id("Keeper"), f.id("Keeper")
	for _, k := range []reasonr.Value{k1, k2} {
		f.command("", actor(k), k, "Join", f.a.String("x"), reasonr.None).Release()
	}
	sub1, _ := f.k.Subscribe(context.Background(), tenant, f.a, k1)
	sub2, _ := f.k.Subscribe(context.Background(), tenant, f.a, k2)
	defer sub1.Close()
	defer sub2.Close()
	res := f.command("", actor(k1), k1, "Poke", k2)
	if len(res.Notifications) != 1 || res.Notifications[0].Name != f.method(k1.Type, "pokeSent") ||
		!reasonr.Equal(res.Arena, res.Notifications[0].To, f.a, k1) {
		t.Fatalf("in the response: %d notifications", len(res.Notifications))
	}
	res.Release()
	select {
	case d := <-sub2.C():
		if d.Notification.Name != f.method(k1.Type, "poked") {
			t.Fatalf("to another actor: %d", d.Notification.Name)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("not delivered to another actor")
	}
	select {
	case d := <-sub1.C():
		t.Fatalf("to the initiator by subscription: %d", d.Notification.Name)
	case <-time.After(30 * time.Millisecond):
	}
}

// arm sets the timer Box.expire in a minute and returns its lease at the
// moment it fires.
func (f *fixture) arm() kind.LeasedTimer {
	f.t.Helper()
	b := f.id("Box")
	f.put(b, 1)
	f.command("", admin, b, "Arm", reasonr.Duration(time.Minute)).Release()
	f.clock.Set(tenant, start.Add(time.Minute))
	ts, err := f.store.Lease(context.Background(), port.LeaseRequest{Tenant: tenant, Now: start.Add(time.Minute), Limit: 1, TTL: time.Minute})
	if err != nil || len(ts) != 1 {
		f.t.Fatalf("lease: %d %v", len(ts), err)
	}
	return ts[0]
}

// TestTimerRejected: a business error of a timer command is a result: the
// timer is deleted, the effects are dropped.
func TestTimerRejected(t *testing.T) {
	f := newFixture(t, kernel.Config{}, false)
	lt := f.arm()
	f.interp.reject = true
	res, err := f.k.Fire(context.Background(), &lt)
	if err != nil || res == nil || len(res.Errors) != 1 || res.Errors[0].Name != "cannotExpire" {
		t.Fatalf("Fire: %v", err)
	}
	res.Release()
	if live, dead := f.store.Timers(tenant); live != 0 || dead != 0 {
		t.Fatalf("the timer is not deleted: %d live, %d dead", live, dead)
	}
}

// TestTimerFault: a fault is retried with a pause of 1, 2, 4… s; after
// TimerAttempts faults the timer is dead; once fixed, a revived timer fires.
func TestTimerFault(t *testing.T) {
	f := newFixture(t, kernel.Config{TimerAttempts: 2}, false)
	lt := f.arm()
	f.interp.fault = true
	if _, err := f.k.Fire(context.Background(), &lt); !errors.Is(err, errFault) {
		t.Fatalf("first fault: %v", err)
	}
	lease := func(at time.Time) []kind.LeasedTimer {
		ts, err := f.store.Lease(context.Background(), port.LeaseRequest{Tenant: tenant, Now: at, Limit: 1, TTL: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
	now := start.Add(time.Minute)
	if ts := lease(now.Add(time.Second - time.Nanosecond)); len(ts) != 0 {
		t.Fatal("a retry before the pause")
	}
	ts := lease(now.Add(time.Second))
	if len(ts) != 1 || ts[0].Attempts != 2 {
		t.Fatalf("a retry after 1 s: %+v", ts)
	}
	if _, err := f.k.Fire(context.Background(), &ts[0]); !errors.Is(err, errFault) {
		t.Fatalf("second fault: %v", err)
	}
	if live, dead := f.store.Timers(tenant); live != 0 || dead != 1 {
		t.Fatalf("after the second fault: %d live, %d dead", live, dead)
	}
	f.interp.fault = false
	if n, _ := f.store.Revive(context.Background(), tenant); n != 1 {
		t.Fatalf("revived %d", n)
	}
	ts = lease(now.Add(time.Hour))
	if len(ts) != 1 {
		t.Fatal("the revived timer is not selected")
	}
	res, err := f.k.Fire(context.Background(), &ts[0])
	if err != nil || res == nil || len(res.Errors) != 0 {
		t.Fatalf("after the fix: %v", err)
	}
	res.Release()
	if live, dead := f.store.Timers(tenant); live != 0 || dead != 0 {
		t.Fatalf("after firing: %d live, %d dead", live, dead)
	}
}

func TestBackoff(t *testing.T) {
	for attempts, want := range map[int]time.Duration{0: time.Second, 1: time.Second, 2: 2 * time.Second, 4: 8 * time.Second, 11: 10 * time.Minute} {
		if got := kernel.Backoff(attempts); got != want {
			t.Errorf("Backoff(%d) = %v, want %v", attempts, got, want)
		}
	}
}

// TestIdempotencyRace: two concurrent commands with one key — the second
// commit gets ErrDuplicateKey, and the stored response is taken.
func TestIdempotencyRace(t *testing.T) {
	var f *fixture
	b := reasonr.None
	inner := false
	f = newFixture(t, kernel.Config{BeforeCommit: func(*reasonr.UnitOfWork) {
		if b.Kind == reasonr.KindNone || inner {
			return
		}
		inner = true
		f.command("touch-1", reasonr.Caller{Kind: reasonr.CallerTimer}, b, "touch").Release()
	}}, false)
	id := f.id("Box")
	f.put(id, 1)
	b = id
	res := f.command("touch-1", reasonr.Caller{Kind: reasonr.CallerTimer}, b, "touch")
	defer res.Release()
	if !res.Replayed || res.Position == 0 {
		t.Fatalf("the response of the losing commit: replayed=%v position %d", res.Replayed, res.Position)
	}
	st, v, _, _ := f.store.Load(context.Background(), tenant, f.a, b.Type, b)
	if v != 2 || f.a.Fields(st)[f.s.Type(b.Type).Field("touches").Ref].Int() != 1 {
		t.Fatalf("the command was applied twice: version %d", v)
	}
}

func TestUnknownTenant(t *testing.T) {
	f := newFixture(t, kernel.Config{}, false)
	b := f.id("Box")
	_, err := f.k.Command(context.Background(), &kind.CommandRequest{Tenant: "nope", Call: reasonr.CommandCall{
		Entity: b.Type, ID: b, Command: f.method(b.Type, "touch"), Args: f.a.Record(0), Arena: f.a, Caller: admin,
	}})
	if !errors.Is(err, port.ErrUnknownTenant) {
		t.Fatalf("expected ErrUnknownTenant: %v", err)
	}
}

// TestOnRequests: a commit with an external request wakes the sender
// (Config.OnRequests), one without requests does not.
func TestOnRequests(t *testing.T) {
	var woken int
	f := newFixture(t, kernel.Config{OnRequests: func() { woken++ }}, false)
	b := f.id("Box")
	f.put(b, 1)
	if woken != 0 {
		t.Fatalf("Put without requests woke the sender %d times", woken)
	}
	res := f.command("", admin, b, "Ship", reasonr.Duration(time.Hour))
	defer res.Release()
	if len(res.Errors) > 0 || woken != 1 {
		t.Fatalf("Ship: errors %v, woken %d times", res.Errors, woken)
	}
}
