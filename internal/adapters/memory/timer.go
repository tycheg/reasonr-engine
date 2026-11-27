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

// Lease selects fired timers and leases them (port.TimerSource). A timer's
// values do not change, so LeasedTimer refers to its arena; a timer with
// personal arguments gets a copy with the decrypted ones.
func (s *Store) Lease(ctx context.Context, req port.LeaseRequest) ([]kind.LeasedTimer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	type due struct {
		tenant port.Tenant
		t      *timer
	}
	var found []due
	for tenant, sp := range s.spaces {
		if req.Tenant != "" && tenant != req.Tenant {
			continue
		}
		for _, t := range sp.timers {
			at, lease := t.at, t.leaseUntil
			if req.Scenario { // by the scenario clock: the moment and the lease with the timer's offset
				at, lease = at.Add(t.offset), lease.Add(t.offset)
			}
			if t.dead == "" && !at.After(req.Now) && !lease.After(req.Now) {
				found = append(found, due{tenant, t})
			}
		}
	}
	sort.Slice(found, func(i, j int) bool {
		x, y := found[i].t, found[j].t
		xa, ya := x.at.Add(x.offset), y.at.Add(y.offset)
		return xa.Before(ya) || xa.Equal(ya) && x.order < y.order
	})
	if req.Limit > 0 && len(found) > req.Limit {
		found = found[:req.Limit]
	}
	out := make([]kind.LeasedTimer, len(found))
	for i, d := range found {
		t := d.t
		t.leaseUntil = req.Now.Add(req.TTL)
		if req.Scenario {
			t.leaseUntil = t.leaseUntil.Add(-t.offset)
		}
		t.attempts++
		out[i] = kind.LeasedTimer{
			Tenant: d.tenant, ID: t.id, At: t.at.Add(t.offset), Offset: t.offset, Entity: t.entity, EntityID: t.entityID,
			Command: t.command, Args: t.args, Attempts: t.attempts, Arena: t.a,
		}
		if t.sealed != nil {
			if err := s.openTimer(ctx, &out[i], t.sealed); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// openTimer copies the arguments of a leased timer into a new arena and
// decrypts them; called under the lock.
func (s *Store) openTimer(ctx context.Context, lt *kind.LeasedTimer, sealed []byte) error {
	sp := s.spaces[lt.Tenant]
	ti := sp.schema.Type(lt.Entity)
	mi := &ti.Methods[lt.Command]
	src, a := lt.Arena, reasonr.NewArena()
	args := a.Copy(src, lt.Args.Value())
	pc := &storekit.PayloadCrypto{Crypto: sp.crypto, Sealer: s.ring.Sealer(lt.Tenant)}
	if err := pc.OpenArgs(ctx, a, ti.Name+"."+mi.Name, mi.Params, args, sealed); err != nil {
		return err
	}
	lt.Arena, lt.ID, lt.EntityID, lt.Args = a, a.Copy(src, lt.ID), a.Copy(src, lt.EntityID), args.Record()
	return nil
}

// timer returns the scheduled timer of a leased one or nil.
func (s *Store) timer(lt *kind.LeasedTimer) *timer {
	sp := s.spaces[lt.Tenant]
	if sp == nil {
		return nil
	}
	return sp.timers[lt.ID.UUID()]
}

// Retry releases the lease after a fault (port.TimerSource).
func (s *Store) Retry(_ context.Context, lt *kind.LeasedTimer, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.timer(lt); t != nil {
		t.leaseUntil = at
	}
	return nil
}

// Dead makes a timer dead (port.TimerSource).
func (s *Store) Dead(_ context.Context, lt *kind.LeasedTimer, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t := s.timer(lt); t != nil {
		t.dead = reason
	}
	return nil
}

// Revive brings a tenant's dead timers back to work (reasonrd timers
// retry); tenant "" — of all. It returns their number.
func (s *Store) Revive(_ context.Context, tenant port.Tenant) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for name, sp := range s.spaces {
		if tenant != "" && name != tenant {
			continue
		}
		for _, t := range sp.timers {
			if t.dead != "" {
				t.dead, t.attempts, t.leaseUntil = "", 0, time.Time{}
				n++
			}
		}
	}
	return n, nil
}

// Timers returns the number of a tenant's live and dead timers.
func (s *Store) Timers(tenant port.Tenant) (live, dead int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sp := s.spaces[tenant]; sp != nil {
		for _, t := range sp.timers {
			if t.dead != "" {
				dead++
			} else {
				live++
			}
		}
	}
	return live, dead
}

// Due reports whether a tenant that skip does not skip has a live unleased
// timer with a moment no later than now: an engine kind that drives the
// clock of a memory store advances it only when timers have something to
// run.
func (s *Store) Due(now time.Time, skip func(port.Tenant) bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for tenant, sp := range s.spaces {
		if skip != nil && skip(tenant) {
			continue
		}
		for _, t := range sp.timers {
			if t.dead == "" && !t.at.After(now) && !t.leaseUntil.After(now) {
				return true
			}
		}
	}
	return false
}
