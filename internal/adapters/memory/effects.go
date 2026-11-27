package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/internal/storekit"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Read returns the units of work from the outbox after the position from
// (port.EffectSource). The values are valid while the outbox entry lives.
func (s *Store) Read(ctx context.Context, from port.Position, limit int) ([]kind.CommittedUnit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Outbox positions go in a row from 1, but units of work without model
	// effects get no position: a binary search.
	lo, hi := 0, len(s.outbox)
	for lo < hi {
		m := (lo + hi) / 2
		if s.outbox[m].unit.Position <= from {
			lo = m + 1
		} else {
			hi = m
		}
	}
	end := len(s.outbox)
	if limit > 0 && end-lo > limit {
		end = lo + limit
	}
	out := make([]kind.CommittedUnit, 0, end-lo)
	for i := lo; i < end; i++ {
		cu, err := s.openEntry(ctx, &s.outbox[i])
		if err != nil {
			return nil, err
		}
		out = append(out, cu)
	}
	return out, nil
}

// Wait waits for an outbox entry with a position greater than after (port.EffectSource).
func (s *Store) Wait(ctx context.Context, after port.Position) error {
	for {
		s.mu.Lock()
		if s.position > after {
			s.mu.Unlock()
			return nil
		}
		wake := s.wake
		s.mu.Unlock()
		select {
		case <-wake:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Position returns the position of the last committed unit of work.
func (s *Store) Position() port.Position {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.position
}

// Apply applies projection operations from the effect stream
// (port.ProjectionStore): this is how Store serves as an asynchronous
// projection store. The row version is the entry position (RFC §16): an
// operation applies only if pos is greater than the row version or its
// deletion mark, so positions apply in any order and a repeat changes
// nothing. A deletion leaves a mark with the version.
func (s *Store) Apply(ctx context.Context, tenant port.Tenant, pos port.Position, a *reasonr.Arena, ops []reasonr.ProjectionOp,
	keys []port.KeyVersion, forget kind.Forget) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.space(tenant)
	if err != nil {
		return err
	}
	seal := s.ring.Resealer(tenant, keys)
	// The unit's arena is shared with the other consumers of the effect
	// stream, which read it at the same time, and building and sealing a
	// row writes to its arena: the delivery works on copies in its own.
	a, ops, forget.Subjects = storekit.Own(a, ops, forget.Subjects)
	for _, op := range storekit.LastOps(a, ops) {
		r, k, err := sp.row(a, op.Projection, op.ID)
		if err != nil {
			return err
		}
		rk := rowKey{op.Projection, k}
		version := sp.deleted[rk]
		if r != nil {
			version = max(version, r.version)
		}
		if uint64(pos) <= version {
			continue
		}
		p := &pending{t: op.Projection, key: k, id: op.ID, deleted: true}
		if op.Kind == reasonr.ProjectionUpsert {
			if p.state, err = storekit.ProjectionRow(sp.codec, a, &op); err != nil {
				return err
			}
			p.deleted = false
			if err := sp.sealPending(ctx, seal, a, p); err != nil {
				return err
			}
		}
		if err := sp.apply(a, p, pos); err != nil {
			return err
		}
		if p.deleted {
			sp.deleted[rk] = uint64(pos)
			continue
		}
		delete(sp.deleted, rk)
		sp.rows[p.t][k].version = uint64(pos)
	}
	if len(forget.Subjects) > 0 {
		refs, err := sp.refs(a, forget.Subjects)
		if err != nil {
			return err
		}
		if err := sp.erase(refs, forget.Types); err != nil {
			return err
		}
	}
	sp.applied = max(sp.applied, pos)
	return nil
}

// Applied returns the last applied position (port.ProjectionStore).
func (s *Store) Applied(_ context.Context, tenant port.Tenant) (port.Position, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.space(tenant)
	if err != nil {
		return 0, err
	}
	return sp.applied, nil
}

// Events is an in-memory event store (ports.EventStore): the events of units
// of work by tenant; a repeated unit by position is skipped. The store
// journal (Store.Append) keeps personal arguments as none and their
// ciphertexts next to them, and does not decrypt them (RFC §20).
type Events struct {
	mu     sync.Mutex
	units  map[port.Tenant][]kind.CommittedUnit
	seen   map[port.Position]bool
	sealed [][]byte // the ciphertexts of personal arguments
}

var _ ports.EventStore = (*Events)(nil)

// NewEvents returns an empty journal.
func NewEvents() *Events {
	return &Events{units: map[port.Tenant][]kind.CommittedUnit{}, seen: map[port.Position]bool{}}
}

// Append appends the events of units of work (ports.EventStore).
func (j *Events) Append(_ context.Context, units []kind.CommittedUnit) error {
	j.append(units, nil)
	return nil
}

// append appends the events of units of work and the ciphertexts of their
// personal arguments (sealed — by unit).
func (j *Events) append(units []kind.CommittedUnit, sealed [][]byte) {
	j.mu.Lock()
	defer j.mu.Unlock()
	for ui, u := range units {
		if j.seen[u.Position] || len(u.Entities) == 0 {
			continue
		}
		j.seen[u.Position] = true
		if sealed != nil && sealed[ui] != nil {
			j.sealed = append(j.sealed, sealed[ui])
		}
		a := reasonr.NewArena()
		cu := kind.CommittedUnit{Tenant: u.Tenant, Position: u.Position, At: u.At, Arena: a}
		for _, ee := range u.Entities {
			ee.ID = a.Copy(u.Arena, ee.ID)
			evs := make([]reasonr.Event, len(ee.Events))
			for i, ev := range ee.Events {
				ev.Args = a.Copy(u.Arena, ev.Args.Value()).Record()
				evs[i] = ev
			}
			ee.Events = evs
			cu.Entities = append(cu.Entities, ee)
		}
		j.units[u.Tenant] = append(j.units[u.Tenant], cu)
	}
}

// dump appends to b everything the journal keeps.
func (j *Events) dump(b []byte) []byte {
	j.mu.Lock()
	defer j.mu.Unlock()
	for _, us := range j.units {
		for _, u := range us {
			for _, ee := range u.Entities {
				for _, ev := range ee.Events {
					b = u.Arena.AppendFormat(b, ev.Args.Value())
				}
			}
		}
	}
	for _, x := range j.sealed {
		b = append(b, x...)
	}
	return b
}

// between returns the units of work of every tenant with positions in
// (after, upto] in position order.
func (j *Events) between(after, upto port.Position) []kind.CommittedUnit {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []kind.CommittedUnit
	for _, us := range j.units {
		for _, u := range us {
			if u.Position > after && u.Position <= upto {
				out = append(out, u)
			}
		}
	}
	sort.Slice(out, func(i, k int) bool { return out[i].Position < out[k].Position })
	return out
}

// Units returns the tenant's written units of work in order.
func (j *Events) Units(tenant port.Tenant) []kind.CommittedUnit {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]kind.CommittedUnit(nil), j.units[tenant]...)
}

// Clock is an in-memory clock: one per tenant, set by hand (port.Clock). A
// tenant without its own clock sees the start time.
type Clock struct {
	mu    sync.Mutex
	start time.Time
	now   map[port.Tenant]time.Time
}

var _ port.Clock = (*Clock)(nil)

// NewClock returns a clock showing start (in UTC).
func NewClock(start time.Time) *Clock {
	return &Clock{start: start.UTC(), now: map[port.Tenant]time.Time{}}
}

// Now returns the tenant's time.
func (c *Clock) Now(t port.Tenant) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now, ok := c.now[t]; ok {
		return now
	}
	return c.start
}

// Set sets the tenant's clock.
func (c *Clock) Set(t port.Tenant, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now[t] = now.UTC()
}
