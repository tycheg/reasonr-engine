package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/tycheg/reasonr-engine/port"
)

// Coordinator holds cursors, leases and dead letters in memory
// (port.Coordinator).
type Coordinator struct {
	mu      sync.Mutex
	cursors map[string]port.Position
	leases  map[string]lease
	dead    []port.DeadLetter
}

type lease struct {
	holder string
	until  time.Time
}

var _ port.Coordinator = (*Coordinator)(nil)

// NewCoordinator returns an empty state.
func NewCoordinator() *Coordinator {
	return &Coordinator{cursors: map[string]port.Position{}, leases: map[string]lease{}}
}

func (c *Coordinator) Cursor(_ context.Context, consumer string) (port.Position, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cursors[consumer], nil
}

func (c *Coordinator) SetCursor(_ context.Context, consumer string, pos port.Position) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cursors[consumer] = pos
	return nil
}

func (c *Coordinator) Acquire(_ context.Context, name, holder string, now time.Time, ttl time.Duration) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if l, ok := c.leases[name]; ok && l.holder != holder && l.until.After(now) {
		return false, nil
	}
	c.leases[name] = lease{holder: holder, until: now.Add(ttl)}
	return true, nil
}

func (c *Coordinator) Bury(_ context.Context, d port.DeadLetter) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, x := range c.dead {
		if x.Consumer == d.Consumer && x.Position == d.Position && x.Item == d.Item {
			return nil
		}
	}
	c.dead = append(c.dead, d)
	return nil
}

func (c *Coordinator) DeadLetters(_ context.Context, consumer string) ([]port.DeadLetter, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []port.DeadLetter
	for _, d := range c.dead {
		if consumer == "" || d.Consumer == consumer {
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Position < out[j].Position })
	return out, nil
}

func (c *Coordinator) Resurrect(_ context.Context, d port.DeadLetter) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, x := range c.dead {
		if x.Consumer == d.Consumer && x.Position == d.Position && x.Item == d.Item {
			c.dead = append(c.dead[:i], c.dead[i+1:]...)
			return nil
		}
	}
	return nil
}
