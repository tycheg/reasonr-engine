package memory

import (
	"context"
	"sort"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/storekit"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

var _ kind.RequestQueue = (*Store)(nil)

// request is an external request waiting to be sent (port.RequestQueue).
type request struct {
	a        *reasonr.Arena // the outbox entry arena: the values do not change
	position port.Position
	index    int       // the request number in the unit of work
	at       time.Time // the time of the unit of work
	rq       reasonr.ExternalRequest
	sealed   []storekit.Sealed // the ciphertexts of personal arguments
	timer    [16]byte          // the deadline timer; zero — none
	next     time.Time         // not selected earlier: a lease or a retry pause
	attempts int
	last     string // the reason of the last failure
}

// enqueue records the external requests of the committed unit of work e
// (port.RequestQueue): called in Commit under the lock.
func (s *Store) enqueue(sp *space, e *outboxEntry) {
	if len(e.unit.Requests) > 0 && sp.requests == nil {
		sp.requests = map[[16]byte]*request{}
	}
	a := e.unit.Arena
	for i, rq := range e.unit.Requests {
		ti := sp.schema.Type(rq.System)
		r := &request{a: a, position: e.unit.Position, index: i, at: e.unit.At, rq: rq, sealed: e.requests[i]}
		if t := ti.Methods[rq.Request].Timeout(a, rq.Args); t.Kind != reasonr.KindNone {
			r.timer = t.UUID()
		}
		if sp.requests[rq.Key.UUID()] == nil { // the same key is the same request for the recipient
			sp.requests[rq.Key.UUID()] = r
		}
	}
}

// LeaseRequests leases the ready requests of every tenant
// (port.RequestQueue). A request's values do not change, so LeasedRequest
// refers to its arena; a request with personal arguments gets a copy with
// the decrypted ones.
func (s *Store) LeaseRequests(ctx context.Context, now time.Time, limit int, ttl time.Duration) ([]kind.LeasedRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type due struct {
		tenant port.Tenant
		r      *request
	}
	var found []due
	for tenant, sp := range s.spaces {
		for _, r := range sp.requests {
			if !r.next.After(now) {
				found = append(found, due{tenant, r})
			}
		}
	}
	sort.Slice(found, func(i, j int) bool {
		x, y := found[i].r, found[j].r
		switch {
		case !x.next.Equal(y.next):
			return x.next.Before(y.next)
		case x.position != y.position:
			return x.position < y.position
		}
		return x.index < y.index
	})
	if limit > 0 && len(found) > limit {
		found = found[:limit]
	}
	out := make([]kind.LeasedRequest, len(found))
	for i, d := range found {
		r, sp := d.r, s.spaces[d.tenant]
		r.next = now.Add(ttl)
		r.attempts++
		lr := kind.LeasedRequest{LeasedRequest: port.LeasedRequest{Tenant: d.tenant, Position: r.position, At: r.at, Attempts: r.attempts}, Request: r.rq, Arena: r.a}
		if r.timer != ([16]byte{}) {
			if t := sp.timers[r.timer]; t != nil {
				lr.Deadline = t.at
			} else {
				lr.TimerGone = true
			}
		}
		if len(r.sealed) > 0 {
			ti := sp.schema.Type(r.rq.System)
			mi := &ti.Methods[r.rq.Request]
			a := reasonr.NewArena()
			args := a.Copy(r.a, r.rq.Args.Value())
			if err := sp.crypto.Open(ctx, s.ring.Sealer(d.tenant), a, ti.Name+"."+mi.Name, mi.Params, args, r.sealed); err != nil {
				return nil, err
			}
			lr.Arena, lr.Request.Args, lr.Request.Key = a, args.Record(), a.Copy(r.a, r.rq.Key)
		}
		lr.Request.Subjects = nil
		out[i] = lr
	}
	return out, nil
}

// RequestDone deletes a request (port.RequestQueue).
func (s *Store) RequestDone(_ context.Context, lr *kind.LeasedRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sp := s.spaces[lr.Tenant]; sp != nil {
		delete(sp.requests, lr.Request.Key.UUID())
	}
	return nil
}

// RetryRequest schedules the next attempt (port.RequestQueue).
func (s *Store) RetryRequest(_ context.Context, lr *kind.LeasedRequest, at time.Time, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sp := s.spaces[lr.Tenant]; sp != nil {
		if r := sp.requests[lr.Request.Key.UUID()]; r != nil {
			r.next, r.last = at, reason
		}
	}
	return nil
}

// Requests returns the number of the tenant's requests waiting to be sent.
func (s *Store) Requests(tenant port.Tenant) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sp := s.spaces[tenant]; sp != nil {
		return len(sp.requests)
	}
	return 0
}
