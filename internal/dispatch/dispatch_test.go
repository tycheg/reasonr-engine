package dispatch_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/memory"
	"github.com/tycheg/reasonr-engine/internal/conformance"
	"github.com/tycheg/reasonr-engine/internal/dispatch"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

const tenant = port.DefaultTenant

var start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

type fixture struct {
	t        *testing.T
	s        *reasonr.Schema
	programs *kernel.Programs
	store    *memory.Store
	views    *memory.Store
	clock    *memory.Clock
	coord    *memory.Coordinator
	k        *kernel.Kernel
}

func newFixture(t *testing.T) *fixture {
	m := conformance.Machine(t)
	f := &fixture{t: t, s: m.Schema(), programs: kernel.NewPrograms(), clock: memory.NewClock(start), coord: memory.NewCoordinator()}
	f.programs.Set(tenant, m)
	f.store, f.views = memory.NewStore(f.programs), memory.NewStore(f.programs)
	k, err := kernel.New(kernel.Config{Programs: f.programs, Store: f.store, Timers: f.store, Clock: f.clock,
		Async: map[string]kind.ProjectionStore{"BoxView": f.views}, ConsistencyWait: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	f.k = k
	return f
}

func (f *fixture) dispatcher(consumers ...dispatch.Consumer) *dispatch.Dispatcher {
	return &dispatch.Dispatcher{Effects: f.store, Coord: f.coord, Consumers: consumers, LeaseTTL: 300 * time.Millisecond,
		Pause: func(int) time.Duration { return 10 * time.Millisecond }}
}

// run starts the dispatcher and returns the stop.
func run(d *dispatch.Dispatcher) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = d.Run(ctx)
		close(done)
	}()
	return func() {
		cancel()
		<-done
	}
}

func (f *fixture) exec(id reasonr.Value, cmd string, args ...reasonr.Value) port.Position {
	f.t.Helper()
	a := reasonr.NewArena()
	res, err := f.k.Command(context.Background(), &kind.CommandRequest{Tenant: tenant, Call: reasonr.CommandCall{
		Entity: id.Type, ID: id, Command: f.s.Type(id.Type).Method(cmd).Ref, Args: a.Record(0, args...), Arena: a,
		Caller: reasonr.Caller{Kind: reasonr.CallerUser, Roles: []string{"admin"}},
	}})
	if err != nil || len(res.Errors) > 0 {
		f.t.Fatalf("%s: %v", cmd, err)
	}
	defer res.Release()
	return res.Position
}

func (f *fixture) box() reasonr.Value {
	b, _ := f.k.NewID(context.Background(), tenant, f.s.Lookup("Box").Ref)
	return b
}

func (f *fixture) put(b reasonr.Value, size int64) port.Position {
	return f.exec(b, "Put", reasonr.Enum(f.s.Lookup("Kind").Ref, 0), reasonr.Int(size), reasonr.None)
}

// TestProjectionsAfter: the dispatcher fills an asynchronous projection; a
// query with after sees its own command.
func TestProjectionsAfter(t *testing.T) {
	f := newFixture(t)
	stop := run(f.dispatcher(&dispatch.Projections{ConsumerName: "projections:views", Store: f.views, Catalog: f.programs,
		Names: map[string]bool{"BoxView": true}}))
	defer stop()
	view := f.s.Lookup("BoxView")
	for i := range 5 {
		b := f.box()
		pos := f.put(b, int64(i+10))
		a := reasonr.NewArena()
		out, err := f.k.Query(context.Background(), &kind.QueryRequest{Tenant: tenant, After: pos, Call: reasonr.QueryCall{
			Type: view.Ref, Query: view.Method("Size").Ref, Args: a.Record(0, b), Arena: a,
			Caller: reasonr.Caller{Kind: reasonr.CallerUser, Roles: []string{"admin"}}}})
		if err != nil || len(out.Errors) > 0 || out.Arena.Elems(out.Result)[0].Int() != int64(i+10) {
			t.Fatalf("query %d with after: %v", i, err)
		}
		out.Release()
	}
}

// TestRestart: stopping and starting the dispatcher between commits — the
// event journal gets each unit exactly once.
func TestRestart(t *testing.T) {
	f := newFixture(t)
	events := memory.NewEvents()
	consumer := &dispatch.Events{Sink: events}
	b := f.box()
	// An instance gives up the lease on stop: the next does not wait for LeaseTTL.
	instance := func() *dispatch.Dispatcher {
		d := f.dispatcher(consumer)
		d.LeaseTTL = time.Minute
		return d
	}
	stop := run(instance())
	for range 3 {
		f.put(b, 1)
	}
	waitFor(t, func() bool { return len(events.Units(tenant)) == 3 })
	stop()
	for range 4 {
		f.put(b, 2)
	}
	stop = run(instance())
	defer stop()
	waitFor(t, func() bool { return len(events.Units(tenant)) == 7 })
	time.Sleep(50 * time.Millisecond)
	seen := map[port.Position]bool{}
	for _, u := range events.Units(tenant) {
		if seen[u.Position] {
			t.Fatalf("position %d delivered twice", u.Position)
		}
		seen[u.Position] = true
	}
	if len(seen) != 7 {
		t.Fatalf("delivered %d of 7", len(seen))
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("did not wait")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// recorder is an integration service for tests: it answers with codes from a queue.
type recorder struct {
	mu    sync.Mutex
	codes []int
	got   []call
}

type call struct {
	path, key string
	body      map[string]any
	header    http.Header
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, call{req.URL.Path, req.Header.Get("Idempotency-Key"), body, req.Header.Clone()})
	code := 200
	if len(r.codes) > 0 {
		code, r.codes = r.codes[0], r.codes[1:]
	}
	w.WriteHeader(code)
}

func (r *recorder) calls() []call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]call(nil), r.got...)
}

// expire sets a box timer and runs it: the expire command sends the
// external request Store.save.
func (f *fixture) expire(b reasonr.Value) {
	f.exec(b, "Arm", reasonr.Duration(time.Minute))
	f.clock.Set(tenant, start.Add(time.Minute))
	ts, err := f.store.Lease(context.Background(), port.LeaseRequest{Tenant: tenant, Now: start.Add(time.Minute), Limit: 10, TTL: time.Minute})
	if err != nil || len(ts) != 1 {
		f.t.Fatalf("timer: %v", err)
	}
	res, err := f.k.Fire(context.Background(), &ts[0])
	if err != nil || res == nil {
		f.t.Fatalf("Fire: %v", err)
	}
	res.Release()
}

// TestLease: one instance holds the lease until it expires.
func TestLease(t *testing.T) {
	c := memory.NewCoordinator()
	ctx := context.Background()
	if ok, _ := c.Acquire(ctx, "x", "a", start, time.Minute); !ok {
		t.Fatal("the first lease")
	}
	if ok, _ := c.Acquire(ctx, "x", "b", start.Add(time.Second), time.Minute); ok {
		t.Fatal("another's lease before expiry")
	}
	if ok, _ := c.Acquire(ctx, "x", "a", start.Add(time.Second), time.Minute); !ok {
		t.Fatal("extending one's own lease")
	}
	if ok, _ := c.Acquire(ctx, "x", "b", start.Add(2*time.Minute), time.Minute); !ok {
		t.Fatal("a lease after expiry")
	}
}
