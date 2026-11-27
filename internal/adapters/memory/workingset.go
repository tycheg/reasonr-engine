package memory

import (
	"context"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/port"
)

// The working set of a memory store: only the entities that were accessed
// are in memory; clean entities are evicted, and a miss is hydrated from
// the database.

// RowKey returns the row key of the entity t with the identifier id.
func (s *Store) RowKey(tenant port.Tenant, a *reasonr.Arena, t reasonr.TypeRef, id reasonr.Value) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.space(tenant)
	if err != nil {
		return "", err
	}
	return sp.key(a, t, id)
}

// Has reports whether an entity row is in memory.
func (s *Store) Has(tenant port.Tenant, a *reasonr.Arena, t reasonr.TypeRef, id reasonr.Value) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.space(tenant)
	if err != nil {
		return false, err
	}
	r, _, err := sp.row(a, t, id)
	return r != nil, err
}

// UniqueKey returns the key of the unique index in of the record state of
// the entity t and whether a row in memory takes it. ok == false — the key
// has none or a personal field: such a key is not checked here.
func (s *Store) UniqueKey(tenant port.Tenant, a *reasonr.Arena, t reasonr.TypeRef, in *reasonr.IndexInfo, state, id reasonr.Value) (key string, taken, ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp, err := s.space(tenant)
	if err != nil {
		return "", false, false, err
	}
	for _, k := range in.Keys {
		if sp.crypto.Personal(t, k.Field) {
			return "", false, false, nil
		}
	}
	uk, ok, err := sp.uniqueKey(a, t, in, state, id, nil)
	if err != nil || !ok {
		return "", false, false, err
	}
	_, taken = sp.uniq[uk]
	return uk.key, taken, true, nil
}

// Evict removes from memory the entity rows for which evict returns true;
// last is the position of the row's last change. It returns the keys of
// the removed rows by type.
func (s *Store) Evict(_ context.Context, tenant port.Tenant, evict func(t reasonr.TypeRef, key string, last port.Position) bool) map[reasonr.TypeRef][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	sp := s.spaces[tenant]
	if sp == nil {
		return nil
	}
	out := map[reasonr.TypeRef][]string{}
	for t, tab := range sp.rows {
		ti := sp.schema.Type(t)
		if ti == nil || ti.Kind != reasonr.TypeEntity && ti.Kind != reasonr.TypeActor {
			continue // projections and fakes of external systems are not evicted
		}
		for key, r := range tab {
			var last port.Position
			if n := len(r.events); n > 0 {
				last = r.events[n-1].Position
			}
			if !evict(t, key, last) {
				continue
			}
			sp.unindex(r)
			r.sealed, r.blind = nil, nil
			sp.setSubjects(r)
			delete(tab, key)
			out[t] = append(out[t], key)
		}
	}
	return out
}

// StartPositions sets the position after which the store numbers units of
// work: the positions of several stores of one engine do not overlap.
// Called before the first commit.
func (s *Store) StartPositions(after port.Position) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.position = max(s.position, after)
}
