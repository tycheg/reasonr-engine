package memory

import (
	"context"
	"sort"
	"time"

	"github.com/tycheg/reasonr-engine/port"
)

var _ port.ForgetLog = (*Store)(nil)

// Record appends forget journal entries (port.ForgetLog).
func (s *Store) Record(_ context.Context, entries []port.Forgotten) error {
	s.mu.Lock()
	defer s.mu.Unlock()
next:
	for _, e := range entries {
		for _, o := range s.forgets {
			if o.Tenant == e.Tenant && o.Position == e.Position && o.Subject == e.Subject {
				continue next
			}
		}
		e.At = e.At.UTC()
		s.forgets = append(s.forgets, e)
	}
	sort.SliceStable(s.forgets, func(i, j int) bool { return s.forgets[i].Position < s.forgets[j].Position })
	return nil
}

// Forgotten returns the forget journal entries at or after since
// (port.ForgetLog).
func (s *Store) Forgotten(_ context.Context, since time.Time) ([]port.Forgotten, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []port.Forgotten
	for _, e := range s.forgets {
		if !e.At.Before(since) {
			out = append(out, e)
		}
	}
	return out, nil
}
