package conformance

import (
	"context"
	"testing"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// lease leases the tenant's timers fired by the moment now (all — of every tenant).
func (f *fixture) lease(tenant port.Tenant, now time.Time, ttl time.Duration) []kind.LeasedTimer {
	f.t.Helper()
	ts, err := f.st.Timers.Lease(context.Background(), port.LeaseRequest{Tenant: tenant, Now: now, Limit: 100, TTL: ttl})
	if err != nil {
		f.t.Fatal(err)
	}
	return ts
}

// advance runs the tenant's timers with a moment no later than until one by
// one, moving the clock to each moment, and returns the ids of the fired entities.
func (f *fixture) advance(until time.Time) []reasonr.Value {
	f.t.Helper()
	var fired []reasonr.Value
	for {
		ts, err := f.st.Timers.Lease(context.Background(), port.LeaseRequest{Tenant: f.tenant, Now: until, Limit: 1, TTL: kernel.TimerLease})
		if err != nil {
			f.t.Fatal(err)
		}
		if len(ts) == 0 {
			break
		}
		if ts[0].At.After(f.clock.Now(f.tenant)) {
			f.clock.set(ts[0].At)
		}
		res, err := f.k.Fire(context.Background(), &ts[0])
		if err != nil {
			f.t.Fatal(err)
		}
		if res != nil {
			fired = append(fired, f.a.Copy(ts[0].Arena, ts[0].EntityID))
			res.Release()
		}
	}
	f.clock.set(until)
	return fired
}

// testTimers: timers fire by moment and scheduling order, their commands
// are committed, a deleted timer does not fire.
func testTimers(t *testing.T, sf Factory) {
	f := newFixture(t, sf, options{})
	b1, b2 := f.id("Box"), f.id("Box")
	for _, b := range []reasonr.Value{b1, b2} {
		f.exec(admin, b, "Put", f.kind("small"), reasonr.Int(1), reasonr.None)
	}
	f.exec(admin, b2, "Arm", reasonr.Duration(30*time.Second))
	f.exec(admin, b1, "Arm", reasonr.Duration(time.Minute))
	before := lastPosition(f)
	if fired := f.advance(Start.Add(29 * time.Second)); len(fired) != 0 {
		t.Fatalf("fired too early: %d", len(fired))
	}
	fired := f.advance(Start.Add(time.Minute))
	if len(fired) != 2 || !reasonr.Equal(f.a, fired[0], f.a, b2) || !reasonr.Equal(f.a, fired[1], f.a, b1) {
		t.Fatalf("%d timers fired out of order", len(fired))
	}
	if _, _, ok := f.load(reasonr.ID(f.typ("BoxView"), b1), "BoxView"); ok {
		t.Fatal("the timer did not delete the projection row")
	}
	st, _, _ := f.load(b1, "Box")
	if f.field(st, "Box", "timer").Kind != reasonr.KindNone {
		t.Fatal("the timer is not reset")
	}
	requests := 0
	for _, u := range f.units(before) {
		requests += len(u.Requests)
	}
	if requests != 2 {
		t.Fatalf("%d external requests in the outbox, want 2", requests)
	}
	// A deleted timer does not fire.
	f.exec(admin, b1, "Arm", reasonr.Duration(time.Second))
	f.exec(admin, b1, "Disarm")
	if fired := f.advance(Start.Add(time.Hour)); len(fired) != 0 {
		t.Fatalf("a deleted timer fired")
	}
	if ts := f.lease("", Start.Add(48*time.Hour), time.Second); len(ts) != 0 {
		t.Fatalf("timers left after firing: %d", len(ts))
	}
}

// testTimerLease: a leased timer is not selected until the lease ends,
// after a fault — until the retry moment, a dead one — never.
func testTimerLease(t *testing.T, sf Factory) {
	f := newFixture(t, sf, options{})
	ctx := context.Background()
	b := f.id("Box")
	f.exec(admin, b, "Put", f.kind("small"), reasonr.Int(1), reasonr.None)
	f.exec(admin, b, "Arm", reasonr.Duration(time.Minute))
	now := Start.Add(2 * time.Minute)
	ts := f.lease(T1, now, 30*time.Second)
	if len(ts) != 1 || ts[0].Attempts != 1 || !ts[0].At.Equal(Start.Add(time.Minute)) || ts[0].Tenant != T1 {
		t.Fatalf("lease: %+v", ts)
	}
	if ts[0].Entity != f.typ("Box") || ts[0].Command != f.method(ts[0].Entity, "expire") || !reasonr.Equal(ts[0].Arena, ts[0].EntityID, f.a, b) {
		t.Fatal("lease: a wrong command")
	}
	if again := f.lease(T1, now.Add(29*time.Second), 30*time.Second); len(again) != 0 {
		t.Fatal("a leased timer was selected again before the lease ended")
	}
	again := f.lease(T1, now.Add(31*time.Second), 30*time.Second)
	if len(again) != 1 || again[0].Attempts != 2 {
		t.Fatalf("after the lease ended: %+v", again)
	}
	retry := now.Add(10 * time.Minute)
	if err := f.st.Timers.Retry(ctx, &again[0], retry); err != nil {
		t.Fatal(err)
	}
	if ts := f.lease(T1, retry.Add(-time.Second), 30*time.Second); len(ts) != 0 {
		t.Fatal("the timer was selected before the retry moment")
	}
	ts = f.lease(T1, retry, 30*time.Second)
	if len(ts) != 1 || ts[0].Attempts != 3 {
		t.Fatalf("at the retry moment: %+v", ts)
	}
	if err := f.st.Timers.Dead(ctx, &ts[0], "fault"); err != nil {
		t.Fatal(err)
	}
	if ts := f.lease(T1, retry.Add(time.Hour), 30*time.Second); len(ts) != 0 {
		t.Fatal("a dead timer was selected")
	}
}

// testTimerGone: the command of a timer that is gone is not committed.
func testTimerGone(t *testing.T, sf Factory) {
	f := newFixture(t, sf, options{})
	b := f.id("Box")
	f.exec(admin, b, "Put", f.kind("small"), reasonr.Int(1), reasonr.None)
	f.exec(admin, b, "Arm", reasonr.Duration(time.Minute))
	f.clock.set(Start.Add(time.Minute))
	ts := f.lease(T1, Start.Add(time.Minute), time.Second)
	if len(ts) != 1 {
		t.Fatalf("%d timers", len(ts))
	}
	// Another instance ran the timer after the lease ended.
	res, err := f.k.Fire(context.Background(), &ts[0])
	if err != nil || res == nil {
		t.Fatalf("the first firing: %v", err)
	}
	res.Release()
	before := lastPosition(f)
	res, err = f.k.Fire(context.Background(), &ts[0])
	if err != nil || res != nil {
		t.Fatalf("a repeated firing: %v, response %v", err, res != nil)
	}
	if n := len(f.units(before)); n != 0 {
		t.Fatalf("a repeated firing was committed: %d", n)
	}
	// A command deleted the timer while it was leased.
	f.exec(admin, b, "Put", f.kind("small"), reasonr.Int(1), reasonr.None)
	f.exec(admin, b, "Arm", reasonr.Duration(time.Minute))
	ts = f.lease(T1, Start.Add(time.Hour), time.Second)
	f.exec(admin, b, "Disarm")
	res, err = f.k.Fire(context.Background(), &ts[0])
	if err != nil || res != nil {
		t.Fatalf("firing a deleted one: %v", err)
	}
	if _, _, ok := f.load(reasonr.ID(f.typ("BoxView"), b), "BoxView"); !ok {
		t.Fatal("the command of a deleted timer was applied")
	}
}

// testTimerTenants: selecting by tenant and across every tenant.
func testTimerTenants(t *testing.T, sf Factory) {
	f := newFixture(t, sf, options{})
	for _, tn := range []port.Tenant{T1, T2} {
		f.tenant = tn
		b := f.id("Box")
		f.exec(admin, b, "Put", f.kind("small"), reasonr.Int(1), reasonr.None)
		f.exec(admin, b, "Arm", reasonr.Duration(time.Minute))
	}
	if ts := f.lease(T2, Start.Add(time.Hour), time.Second); len(ts) != 1 || ts[0].Tenant != T2 {
		t.Fatalf("T2 timers: %+v", ts)
	}
	ts := f.lease("", Start.Add(time.Hour), time.Second)
	if len(ts) != 1 || ts[0].Tenant != T1 {
		t.Fatalf("the timers of every tenant (T2 is leased): %+v", ts)
	}
}

// testTimerOffset: a timer set by a unit of work with a scenario clock
// offset (RFC §27.5) is stored by the real moment: by the real clock it
// fires after its delay, by the scenario clock when the scenario reaches
// its moment; a lease returns the scenario moment and the offset.
func testTimerOffset(t *testing.T, sf Factory) {
	f := newFixture(t, sf, options{})
	b := f.id("Box")
	f.exec(admin, b, "Put", f.kind("small"), reasonr.Int(1), reasonr.None)
	const offset = -30 * 24 * time.Hour
	ctx := port.WithOffset(context.Background(), offset)
	res, err := f.k.Command(ctx, &kind.CommandRequest{Tenant: f.tenant, Call: reasonr.CommandCall{
		Entity: b.Type, ID: b, Command: f.method(b.Type, "Arm"), Args: f.a.Record(0, reasonr.Duration(time.Minute)), Arena: f.a, Caller: admin,
	}})
	if err != nil || len(res.Errors) > 0 {
		t.Fatalf("Arm with an offset: %v %v", err, res)
	}
	res.Release()
	scenario := Start.Add(offset) // the scenario clock at scheduling
	lease := func(now time.Time, sc bool) []kind.LeasedTimer {
		ts, err := f.st.Timers.Lease(context.Background(), port.LeaseRequest{Tenant: f.tenant, Now: now, Limit: 10, TTL: time.Second, Scenario: sc})
		if err != nil {
			t.Fatal(err)
		}
		return ts
	}
	if ts := lease(Start.Add(59*time.Second), false); len(ts) != 0 {
		t.Fatal("by the real clock the timer fired before its minute")
	}
	if ts := lease(scenario.Add(59*time.Second), true); len(ts) != 0 {
		t.Fatal("by the scenario clock the timer fired before its minute")
	}
	ts := lease(scenario.Add(time.Minute), true)
	if len(ts) != 1 || ts[0].Offset != offset || !ts[0].At.Equal(scenario.Add(time.Minute)) {
		t.Fatalf("a lease by the scenario clock: %+v", ts)
	}
	// A retry after a fault — by the real clock; firing — in the scenario clock.
	if err := f.st.Timers.Retry(context.Background(), &ts[0], Start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	ts = lease(Start.Add(time.Minute), false)
	if len(ts) != 1 || !ts[0].At.Equal(scenario.Add(time.Minute)) {
		t.Fatalf("a lease by the real clock: %+v", ts)
	}
	res, err = f.k.Fire(context.Background(), &ts[0])
	if err != nil || res == nil {
		t.Fatalf("Fire: %v", err)
	}
	res.Release()
	if ts := lease(Start.Add(48*time.Hour), false); len(ts) != 0 {
		t.Fatalf("timers left after firing: %d", len(ts))
	}
}
