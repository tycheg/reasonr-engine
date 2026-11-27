package conformance

import (
	"context"
	"sync"
	"testing"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/kind"
)

// RunRequests runs the set of external requests waiting to be sent
// (port.RequestQueue, RFC §18): writing at commit, leasing and its expiry
// (an instance restart), parallel leasing without handing out twice, a
// retry in its time, a deadline timer, tenants. The factory's main store
// must implement port.RequestQueue.
func RunRequests(t *testing.T, f Factory) {
	for _, c := range []struct {
		name string
		run  func(*testing.T, Factory)
	}{
		{"Commit", testRequestCommit}, {"Parallel", testRequestParallel}, {"Retry", testRequestRetry},
		{"Timer", testRequestTimer}, {"Tenants", testRequestTenants},
	} {
		t.Run(c.name, func(t *testing.T) { c.run(t, f) })
	}
}

// queue returns the queue of the main store.
func (f *fixture) queue() kind.RequestQueue {
	f.t.Helper()
	q, ok := f.st.Unit.(kind.RequestQueue)
	if !ok {
		f.t.Fatal("the main store does not implement port.RequestQueue")
	}
	return q
}

// leaseRequests leases the requests ready at the moment now.
func (f *fixture) leaseRequests(now time.Time, ttl time.Duration) []kind.LeasedRequest {
	f.t.Helper()
	rs, err := f.queue().LeaseRequests(context.Background(), now, 100, ttl)
	if err != nil {
		f.t.Fatal(err)
	}
	return rs
}

// ship creates a box and sends Store.ship with a deadline timer after delay.
func (f *fixture) ship(delay time.Duration) reasonr.Value {
	f.t.Helper()
	b := f.id("Box")
	if errs := f.exec(admin, b, "Put", f.kind("small"), reasonr.Int(1), reasonr.None); errs != nil {
		f.t.Fatal(errs)
	}
	if errs := f.exec(admin, b, "Ship", reasonr.Duration(delay)); errs != nil {
		f.t.Fatal(errs)
	}
	return b
}

// boxOf returns the boxId argument of a leased request (in the arena f.a).
func (f *fixture) boxOf(r *kind.LeasedRequest) reasonr.Value {
	return f.a.Copy(r.Arena, r.Arena.Elems(r.Request.Args.Value())[0])
}

// testRequestCommit: a request is written by the commit of a command — with
// the arguments, the key, the position and time of its unit of work; a
// leased one is not handed out until the lease expires (the instance that
// took it stopped); a deleted one is not handed out.
func testRequestCommit(t *testing.T, sf Factory) {
	f := newFixture(t, sf, options{})
	b := f.ship(time.Hour)
	units := f.units(0)
	last := units[len(units)-1]
	rs := f.leaseRequests(Start, time.Minute)
	if len(rs) != 1 {
		t.Fatalf("%d requests leased, want 1", len(rs))
	}
	r := rs[0]
	ti := f.s.Type(r.Request.System)
	if ti.Name != "Store" || ti.Methods[r.Request.Request].Name != "ship" || r.Tenant != T1 || r.Attempts != 1 {
		t.Fatalf("request %s.%s of tenant %s, attempt %d", ti.Name, ti.Methods[r.Request.Request].Name, r.Tenant, r.Attempts)
	}
	if !reasonr.Equal(f.a, f.boxOf(&r), f.a, b) || r.Position != last.Position || !r.At.Equal(last.At) {
		t.Fatalf("a wrong argument, position %d or time %v", r.Position, r.At)
	}
	if !reasonr.Equal(r.Arena, r.Request.Key, last.Arena, last.Requests[0].Key) {
		t.Fatal("a wrong idempotency key")
	}
	if again := f.leaseRequests(Start.Add(59*time.Second), time.Minute); len(again) != 0 {
		t.Fatal("a leased request was handed out again")
	}
	after := f.leaseRequests(Start.Add(time.Minute), time.Minute)
	if len(after) != 1 || after[0].Attempts != 2 {
		t.Fatalf("after the lease expired %d were handed out, want a request with attempt 2", len(after))
	}
	if err := f.queue().RequestDone(context.Background(), &after[0]); err != nil {
		t.Fatal(err)
	}
	if rest := f.leaseRequests(Start.Add(time.Hour), time.Minute); len(rest) != 0 {
		t.Fatal("a deleted request is left")
	}
}

// testRequestParallel: instances leasing at the same time do not get one
// request twice and together take them all.
func testRequestParallel(t *testing.T, sf Factory) {
	f := newFixture(t, sf, options{})
	const total, workers = 40, 8
	for range total {
		f.ship(time.Hour)
	}
	var mu sync.Mutex
	seen := map[[16]byte]int{}
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				rs, err := f.queue().LeaseRequests(context.Background(), Start, 3, time.Minute)
				if err != nil {
					t.Error(err)
					return
				}
				if len(rs) == 0 {
					return
				}
				mu.Lock()
				for _, r := range rs {
					seen[r.Request.Key.UUID()]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(seen) != total {
		t.Fatalf("%d distinct requests leased of %d", len(seen), total)
	}
	for _, n := range seen {
		if n != 1 {
			t.Fatal("a request was handed out twice")
		}
	}
}

// testRequestRetry: a retry postpones a request until its moment and keeps
// the attempt count; a deleted one is not handed out.
func testRequestRetry(t *testing.T, sf Factory) {
	f := newFixture(t, sf, options{})
	f.ship(time.Hour)
	f.ship(time.Hour)
	ctx := context.Background()
	rs := f.leaseRequests(Start, time.Minute)
	if len(rs) != 2 {
		t.Fatalf("%d requests leased", len(rs))
	}
	if err := f.queue().RetryRequest(ctx, &rs[0], Start.Add(10*time.Second), "HTTP 503"); err != nil {
		t.Fatal(err)
	}
	if err := f.queue().RequestDone(ctx, &rs[1]); err != nil {
		t.Fatal(err)
	}
	if got := f.leaseRequests(Start.Add(9*time.Second), time.Minute); len(got) != 0 {
		t.Fatal("a request was handed out before the retry moment")
	}
	got := f.leaseRequests(Start.Add(10*time.Second), time.Minute)
	if len(got) != 1 || got[0].Request.Key.UUID() != rs[0].Request.Key.UUID() || got[0].Attempts != 2 {
		t.Fatalf("at the retry moment %d were handed out, want the first request with attempt 2", len(got))
	}
	if err := f.queue().RetryRequest(ctx, &got[0], Start.Add(20*time.Second), "HTTP 422"); err != nil {
		t.Fatal(err)
	}
	if got := f.leaseRequests(Start.Add(time.Hour), time.Minute); len(got) != 1 || got[0].Attempts != 3 {
		t.Fatalf("the third attempt: %d handed out", len(got))
	}
}

// testRequestTimer: for a request with a deadline timer, leasing reports
// the timer moment while it exists, and that it is gone after a command
// deleted the timer or it fired.
func testRequestTimer(t *testing.T, sf Factory) {
	f := newFixture(t, sf, options{})
	disarmed, fired := f.ship(time.Hour), f.ship(time.Minute)
	rs := f.leaseRequests(Start, time.Second)
	if len(rs) != 2 {
		t.Fatalf("%d requests leased", len(rs))
	}
	for _, r := range rs {
		want := Start.Add(time.Hour)
		if reasonr.Equal(f.a, f.boxOf(&r), f.a, fired) {
			want = Start.Add(time.Minute)
		}
		if r.TimerGone || !r.Deadline.Equal(want) {
			t.Fatalf("deadline timer: gone=%v, moment %v, want %v", r.TimerGone, r.Deadline, want)
		}
	}
	if errs := f.exec(admin, disarmed, "Disarm"); errs != nil {
		t.Fatal(errs)
	}
	if got := f.advance(Start.Add(time.Minute)); len(got) != 1 {
		t.Fatalf("%d timers fired", len(got))
	}
	var ships []kind.LeasedRequest // the command of the fired timer also writes Store.save
	for _, r := range f.leaseRequests(Start.Add(time.Minute), time.Second) {
		if f.s.Type(r.Request.System).Methods[r.Request.Request].Name == "ship" {
			ships = append(ships, r)
		}
	}
	if len(ships) != 2 || !ships[0].TimerGone || !ships[1].TimerGone || !ships[0].Deadline.IsZero() {
		t.Fatalf("after deleting and firing the timers: %d ship requests, no timer — %v", len(ships),
			len(ships) == 2 && ships[0].TimerGone && ships[1].TimerGone)
	}
	// A request without a deadline timer: no moment, and the timer is not "gone".
	b := f.id("Box")
	f.exec(admin, b, "Put", f.kind("small"), reasonr.Int(1), reasonr.None)
	f.exec(admin, b, "Arm", reasonr.Duration(time.Second))
	f.advance(Start.Add(time.Minute + time.Second))
	rs = f.leaseRequests(Start.Add(2*time.Minute), time.Second)
	var save int
	for _, r := range rs {
		if f.s.Type(r.Request.System).Methods[r.Request.Request].Name == "save" {
			save++
			if r.TimerGone || !r.Deadline.IsZero() {
				t.Fatal("a request without a deadline timer has a timer")
			}
		}
	}
	if save != 2 { // from the fired Ship timer and from the Arm timer
		t.Fatalf("save requests: %d", save)
	}
}

// testRequestTenants: leasing hands out the requests of every tenant; a
// request is deleted only in its own tenant.
func testRequestTenants(t *testing.T, sf Factory) {
	f := newFixture(t, sf, options{})
	f.ship(time.Hour)
	f.tenant = T2
	f.ship(time.Hour)
	ctx := context.Background()
	rs := f.leaseRequests(Start, time.Minute)
	if len(rs) != 2 || rs[0].Tenant == rs[1].Tenant {
		t.Fatalf("%d requests of two tenants leased", len(rs))
	}
	done := rs[0]
	if done.Tenant != T1 {
		done = rs[1]
	}
	if err := f.queue().RequestDone(ctx, &done); err != nil {
		t.Fatal(err)
	}
	if rs := f.leaseRequests(Start.Add(time.Hour), time.Minute); len(rs) != 1 || rs[0].Tenant != T2 {
		t.Fatal("what is left is not the request of tenant t2")
	}
}
