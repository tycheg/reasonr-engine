package dispatch_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/tycheg/reasonr-engine/internal/adapters/memory"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// batcher is a consumer with batch delivery; fail — the batch is not
// delivered.
type batcher struct {
	fail bool

	mu      sync.Mutex
	batches []int           // the sizes of the delivered batches
	units   []port.Position // the units delivered one by one
}

func (b *batcher) Name() string { return "batcher" }

func (b *batcher) Deliver(_ context.Context, u *kind.CommittedUnit, _ int, _ func(int, string) error) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.units = append(b.units, u.Position)
	return nil
}

func (b *batcher) DeliverBatch(_ context.Context, units []kind.CommittedUnit) error {
	if b.fail {
		return errors.New("the batch is not written")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.batches = append(b.batches, len(units))
	return nil
}

func (b *batcher) delivered() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(b.units)
	for _, s := range b.batches {
		n += s
	}
	return n
}

// cursors counts the cursor writes.
type cursors struct {
	*memory.Coordinator
	mu  sync.Mutex
	set int
}

func (c *cursors) SetCursor(ctx context.Context, name string, p port.Position) error {
	c.mu.Lock()
	c.set++
	c.mu.Unlock()
	return c.Coordinator.SetCursor(ctx, name, p)
}

// TestBatch: a BatchConsumer gets an outbox batch in one delivery, and the
// cursor moves once; a batch that is not delivered the dispatcher delivers
// unit by unit.
func TestBatch(t *testing.T) {
	for _, fail := range []bool{false, true} {
		f := newFixture(t)
		var last port.Position
		for i := range 10 {
			last = f.put(f.box(), int64(i))
		}
		c := &batcher{fail: fail}
		coord := &cursors{Coordinator: f.coord}
		d := f.dispatcher(c)
		d.Coord = coord
		stop := run(d)
		waitFor(t, func() bool { return c.delivered() >= 10 })
		stop()
		got, _ := f.coord.Cursor(context.Background(), "batcher")
		switch {
		case got != last:
			t.Errorf("fail=%t: cursor %d, want %d", fail, got, last)
		case !fail && (len(c.batches) != 1 || c.batches[0] != 10 || len(c.units) != 0 || coord.set != 1):
			t.Errorf("batches %v, one by one %v, %d cursor writes: want one batch of 10 and one write", c.batches, c.units, coord.set)
		case fail && (len(c.batches) != 0 || len(c.units) != 10):
			t.Errorf("on a batch fault: batches %v, one by one %v — want 10 one by one", c.batches, c.units)
		}
	}
}
