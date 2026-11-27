package dispatch_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/auth"
	"github.com/tycheg/reasonr-engine/internal/adapters/http/integration"
	"github.com/tycheg/reasonr-engine/internal/dispatch"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/kind"
)

// sender is the request sender of the fixture's store with the sender
// clock now and a retry pause of 1 s.
func (f *fixture) sender(t ports.Transport, now *time.Time) *dispatch.Sender {
	return &dispatch.Sender{Queue: f.store, Default: t, Catalog: f.programs, Clock: f.clock, Workers: 4,
		Now: func() time.Time { return *now }, Retry: dispatch.RetryPolicy{Initial: time.Second, Multiplier: 1}}
}

// TestRequests: a request written by a commit is delivered by the sender
// over HTTP with an idempotency key; a fault and a recipient refusal (4xx)
// are retried with the same key until the request is accepted.
func TestRequests(t *testing.T) {
	f := newFixture(t)
	rec := &recorder{codes: []int{503, 422, 200}}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	s := &dispatch.Sender{Queue: f.store, Default: &integration.HTTP{URL: srv.URL, Catalog: f.programs}, Catalog: f.programs,
		Clock: f.clock, Poll: 10 * time.Millisecond, Retry: dispatch.RetryPolicy{Initial: 10 * time.Millisecond}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = s.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	b := f.box()
	f.put(b, 1)
	f.expire(b)
	waitFor(t, func() bool { return len(rec.calls()) == 3 })
	calls := rec.calls()
	if calls[0].path != "/requests/Store/save" || calls[0].key == "" || calls[0].key != calls[1].key || calls[1].key != calls[2].key ||
		calls[2].body["boxId"] == nil {
		t.Fatalf("requests: %+v", calls)
	}
	waitFor(t, func() bool { return f.store.Requests(tenant) == 0 })
}

// TestRetryPolicy: the pause grows with the multiplier up to MaxInterval.
func TestRetryPolicy(t *testing.T) {
	p := dispatch.RetryPolicy{Initial: time.Second, Multiplier: 3, MaxInterval: 20 * time.Second}
	for attempt, want := range map[int]time.Duration{1: time.Second, 2: 3 * time.Second, 3: 9 * time.Second, 4: 20 * time.Second, 40: 20 * time.Second} {
		if got := p.Pause(attempt); got != want {
			t.Errorf("attempt %d: pause %v, want %v", attempt, got, want)
		}
	}
}

// gate is a transport that holds the requests of the box stuck until
// release closes (the system does not answer) and records the others.
type gate struct {
	stuck   reasonr.Value
	release chan struct{}
	mu      sync.Mutex
	sent    int
}

func (g *gate) Send(ctx context.Context, r *kind.LeasedRequest) error {
	if v := r.Arena.Elems(r.Request.Args.Value())[0]; v.Lo == g.stuck.Lo && v.Hi == g.stuck.Hi {
		select {
		case <-g.release:
		case <-ctx.Done():
		}
		return ctx.Err()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sent++
	return nil
}

func (g *gate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sent
}

// TestRequestsIndependent: a request stuck at the recipient does not delay
// the next ones — each is delivered on its own.
func TestRequestsIndependent(t *testing.T) {
	f := newFixture(t)
	var boxes []reasonr.Value
	for range 5 {
		b := f.box()
		f.put(b, 1)
		f.exec(b, "Ship", reasonr.Duration(time.Hour))
		boxes = append(boxes, b)
	}
	g := &gate{stuck: boxes[0], release: make(chan struct{})}
	defer close(g.release)
	s := &dispatch.Sender{Queue: f.store, Default: g, Catalog: f.programs, Clock: f.clock, Workers: 2, Poll: 10 * time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = s.Run(ctx)
		close(done)
	}()
	waitFor(t, func() bool { return g.count() == 4 })
	cancel()
	<-done
}

// TestRequestsTimer: a request with a deadline timer is retried while the
// timer is alive — max_elapsed does not apply to it; a deleted timer stops
// the retries: the request is deleted without a new attempt.
func TestRequestsTimer(t *testing.T) {
	f := newFixture(t)
	b := f.box()
	f.put(b, 1)
	f.exec(b, "Ship", reasonr.Duration(time.Hour))
	fail := &failing{}
	now := start
	s := f.sender(fail, &now)
	s.Retry.MaxElapsed = time.Hour
	ctx := context.Background()
	for range 3 {
		if n, err := s.SendDue(ctx); err != nil || n != 1 {
			t.Fatalf("%d attempts: %v", n, err)
		}
		now = now.Add(time.Second)
	}
	f.clock.Set(tenant, start.Add(48*time.Hour))
	if _, err := s.SendDue(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.store.Requests(tenant); n != 1 {
		t.Fatalf("%d requests: a request with a live timer was dropped", n)
	}
	f.exec(b, "Disarm")
	now = now.Add(time.Second)
	if n, err := s.SendDue(ctx); err != nil || n != 1 {
		t.Fatalf("%d attempts: %v", n, err)
	}
	if fail.count() != 4 {
		t.Fatalf("%d sends, want 4: after the timer is deleted the request is not sent", fail.count())
	}
	if n := f.store.Requests(tenant); n != 0 {
		t.Fatalf("requests left: %d", n)
	}
}

// TestRequestsDropped: a request without a deadline timer is retried until
// max_elapsed has passed since its unit of work, then dropped; a request
// to a system without a transport is dropped at once.
func TestRequestsDropped(t *testing.T) {
	f := newFixture(t)
	b := f.box()
	f.put(b, 1)
	f.expire(b) // Store.save without a timer
	fail := &failing{}
	now := start
	s := f.sender(fail, &now)
	s.Retry.MaxElapsed = time.Hour
	ctx := context.Background()
	if _, err := s.SendDue(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.store.Requests(tenant); n != 1 {
		t.Fatalf("before the deadline: %d requests", n)
	}
	f.clock.Set(tenant, start.Add(2*time.Hour))
	now = now.Add(time.Second)
	if _, err := s.SendDue(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.store.Requests(tenant); n != 0 || fail.count() != 2 {
		t.Fatalf("after the deadline: %d requests, %d sends", n, fail.count())
	}
	b2 := f.box()
	f.put(b2, 1)
	f.clock.Set(tenant, start)
	f.expire(b2)
	s.Default = nil
	if n, err := s.SendDue(ctx); err != nil || n != 1 || f.store.Requests(tenant) != 0 || fail.count() != 2 {
		t.Fatalf("without a transport: %d attempts, %d requests, %v", n, f.store.Requests(tenant), err)
	}
}

// TestRequestsContract: a request over HTTP carries the contract headers
// (RFC §18) and the engine's JWT, which the service checks with a public key
// from JWKS; the deadline of the timeout timer is a header, not a body argument.
func TestRequestsContract(t *testing.T) {
	f := newFixture(t)
	rec := &recorder{codes: []int{503, 200}}
	srv := httptest.NewServer(rec)
	defer srv.Close()
	signer, err := auth.GenerateSigner("engine-1")
	if err != nil {
		t.Fatal(err)
	}
	b := f.box()
	f.put(b, 1)
	f.exec(b, "Ship", reasonr.Duration(time.Hour))
	now := start
	s := f.sender(&integration.HTTP{URL: srv.URL, Signer: signer, Catalog: f.programs}, &now)
	for range 2 {
		if _, err := s.SendDue(context.Background()); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
	}
	calls := rec.calls()
	if len(calls) != 2 {
		t.Fatalf("%d attempts", len(calls))
	}
	keys, err := auth.ParseJWKS(signer.JWKS())
	if err != nil {
		t.Fatal(err)
	}
	verifier := &auth.JWT{Keys: &staticKeys{keys}, Issuer: "engine-1", Audience: "Store"}
	for i, c := range calls {
		h := c.header
		if c.path != "/requests/Store/ship" || c.body["boxId"] == nil || len(c.body) != 1 {
			t.Fatalf("attempt %d: %s %v — a body without timeout", i+1, c.path, c.body)
		}
		if h.Get("X-Reasonr-Tenant") != string(tenant) || h.Get("X-Reasonr-Attempt") != strconv.Itoa(i+1) ||
			h.Get("X-Reasonr-Deadline") != start.Add(time.Hour).Format(time.RFC3339Nano) || h.Get("Content-Type") != "application/json" {
			t.Fatalf("attempt %d: headers %v", i+1, h)
		}
		claims, err := verifier.Verify(context.Background(), auth.ParseAuthorization(h.Get("Authorization")).Token)
		if err != nil || claims["tenant"] != string(tenant) || claims["jti"] != c.key {
			t.Fatalf("attempt %d: JWT %v, %v", i+1, claims, err)
		}
	}
	// A request without a deadline timer has no X-Reasonr-Deadline.
	b2 := f.box()
	f.put(b2, 1)
	f.expire(b2)
	if _, err := s.SendDue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c := rec.calls(); len(c) != 3 || c[2].path != "/requests/Store/save" || c[2].header.Get("X-Reasonr-Deadline") != "" {
		t.Fatalf("a request without a deadline: %+v", c[len(c)-1])
	}
}

// staticKeys are JWT verification keys by kid.
type staticKeys struct{ keys map[string]any }

func (k *staticKeys) Key(_ context.Context, kid, _ string) (any, error) {
	if key := k.keys[kid]; key != nil {
		return key, nil
	}
	return nil, errors.New("no key " + kid)
}

// failing is a transport that always answers with a temporary fault.
type failing struct {
	mu sync.Mutex
	n  int
}

func (f *failing) Send(context.Context, *kind.LeasedRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	return errUnavailable
}

func (f *failing) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

var errUnavailable = errors.New("HTTP 503")
