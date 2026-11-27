// Package dispatch is the effect stream (RFC §30–31): the dispatcher reads
// committed units of work from the outbox by position and hands them to
// the consumers — asynchronous projections, the event store, the forget
// journal. The bus reads the event store (Bus), and Sender sends external
// requests. Each consumer has a cursor in the main store and a lease: one
// instance processes the stream, and the order is kept. Delivery is at
// least once: consumers are idempotent by position or key. An effect the
// recipient rejected becomes a dead letter; reasonrd dispatch retry
// delivers it again.
package dispatch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Consumer is an effect stream consumer.
type Consumer interface {
	// Name is the consumer name: the key of the cursor, the lease and the dead letters.
	Name() string
	// Deliver delivers the effects of a unit of work; only >= 0 — only the
	// effect with this number (retrying a dead letter). An effect that can
	// never be delivered the consumer buries through bury and goes on. An
	// error with port.ErrPermanent sends the whole unit to dead letters;
	// another error — the unit is delivered again after a pause.
	Deliver(ctx context.Context, u *kind.CommittedUnit, only int, bury func(item int, reason string) error) error
}

// BatchConsumer is a consumer that delivers a batch of units of work in one
// operation (the event store): the cursor moves once per batch. If a batch
// is not delivered, the dispatcher delivers its units one by one through
// Deliver — with retries and dead letters.
type BatchConsumer interface {
	Consumer
	DeliverBatch(ctx context.Context, units []kind.CommittedUnit) error
}

// Dispatcher is the effect stream dispatcher.
type Dispatcher struct {
	Effects   kind.EffectSource
	Coord     port.Coordinator
	Consumers []Consumer
	// Holder is the instance name in leases; "" — the host, the process and a random suffix.
	Holder   string
	LeaseTTL time.Duration // 0 — 30 s
	Batch    int           // 0 — 100
	Log      *slog.Logger
	Metrics  port.Metrics
	Now      func() time.Time
	// Pause is the pause before a retry after a fault; nil — RetryPause.
	Pause func(attempt int) time.Duration
}

// RetryPause is the pause before retrying a delivery after a fault: 1, 2,
// 4… s, at most a minute.
func RetryPause(attempt int) time.Duration {
	if attempt > 6 {
		return time.Minute
	}
	return min(time.Second<<attempt, time.Minute)
}

func (d *Dispatcher) defaults() {
	if d.LeaseTTL == 0 {
		d.LeaseTTL = 30 * time.Second
	}
	if d.Batch == 0 {
		d.Batch = 100
	}
	if d.Log == nil {
		d.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if d.Metrics == nil {
		d.Metrics = ports.Nop{}
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Pause == nil {
		d.Pause = RetryPause
	}
	if d.Holder == "" {
		d.Holder = holder()
	}
}

// holder is the instance name in leases: the host, the process and a random suffix.
func holder() string {
	host, _ := os.Hostname()
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s/%d/%s", host, os.Getpid(), hex.EncodeToString(b[:]))
}

// Run processes the stream with every consumer until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context) error {
	d.defaults()
	delivered := d.Metrics.Counter("reasonr_dispatch_delivered_total", "Units of work delivered to a consumer.", "consumer")
	failed := d.Metrics.Counter("reasonr_dispatch_failures_total", "Delivery faults (to be retried).", "consumer")
	dead := d.Metrics.Counter("reasonr_dispatch_dead_total", "Dead letters.", "consumer")
	lag := d.Metrics.Gauge("reasonr_dispatch_idle", "1 — the consumer has caught up with the outbox.", "consumer")
	errs := make(chan error, len(d.Consumers))
	for _, c := range d.Consumers {
		w := &worker{d: d, c: c, delivered: delivered, failed: failed, dead: dead, lag: lag}
		go func() { errs <- w.run(ctx) }()
	}
	var first error
	for range d.Consumers {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
	}
	return first
}

type worker struct {
	d                       *Dispatcher
	c                       Consumer
	delivered, failed, dead port.Counter
	lag                     port.Gauge
}

// run is the consumer loop: the lease, reading, delivery, the cursor.
func (w *worker) run(ctx context.Context) error {
	d, name := w.d, w.c.Name()
	renew := d.LeaseTTL / 3
	attempt := 0
	defer func() { release(d.Coord, "dispatch:"+name, d.Holder, d.Now()) }()
	for ctx.Err() == nil {
		ok, err := d.Coord.Acquire(ctx, "dispatch:"+name, d.Holder, d.Now(), d.LeaseTTL)
		if err != nil || !ok {
			if err != nil {
				d.Log.Warn("dispatcher: the lease is not acquired", "consumer", name, "error", err)
			}
			sleep(ctx, renew)
			continue
		}
		deadline := time.Now().Add(renew)
		cursor, err := d.Coord.Cursor(ctx, name)
		if err != nil {
			d.Log.Warn("dispatcher: the cursor is not read", "consumer", name, "error", err)
			sleep(ctx, renew)
			continue
		}
		for ctx.Err() == nil && time.Now().Before(deadline) {
			units, err := d.Effects.Read(ctx, cursor, d.Batch)
			if err != nil {
				d.Log.Warn("dispatcher: the outbox is not read", "consumer", name, "error", err)
				sleep(ctx, time.Second)
				break
			}
			if len(units) == 0 {
				w.lag.Set(1, name)
				wctx, cancel := context.WithDeadline(ctx, deadline)
				_ = d.Effects.Wait(wctx, cursor)
				cancel()
				continue
			}
			w.lag.Set(0, name)
			if bc, ok := w.c.(BatchConsumer); ok && len(units) > 1 {
				if err := bc.DeliverBatch(ctx, units); err == nil {
					cursor = units[len(units)-1].Position
					if err := d.Coord.SetCursor(ctx, name, cursor); err != nil && ctx.Err() == nil {
						d.Log.Warn("dispatcher: the cursor is not written", "consumer", name, "error", err)
					}
					w.delivered.Add(float64(len(units)), name)
					continue
				}
			}
			for i := range units {
				u := &units[i]
				if err := w.deliver(ctx, u, -1); err != nil {
					w.failed.Add(1, name)
					d.Log.Warn("dispatcher: delivery fault, retrying", "consumer", name, "position", uint64(u.Position),
						"tenant", string(u.Tenant), "attempt", attempt+1, "error", err)
					sleep(ctx, d.Pause(attempt))
					attempt++
					break
				}
				attempt = 0
				cursor = u.Position
				if err := d.Coord.SetCursor(ctx, name, cursor); err != nil {
					if ctx.Err() != nil {
						break // stopping: the batch is reread from the cursor
					}
					d.Log.Warn("dispatcher: the cursor is not written", "consumer", name, "error", err)
				}
				w.delivered.Add(1, name)
			}
			if attempt > 0 {
				break // after a fault — again from the cursor and with a lease
			}
		}
	}
	return nil
}

// release gives up the lease name on stop: a lease until "now" expires at
// once, and another instance continues the stream without waiting for
// LeaseTTL (a restart during a deploy). Acquire does not touch another
// instance's lease.
func release(coord port.Coordinator, name, holder string, now time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _ = coord.Acquire(ctx, name, holder, now, 0)
}

// deliver delivers a unit to a consumer; ErrPermanent — a dead letter of
// the whole unit.
func (w *worker) deliver(ctx context.Context, u *kind.CommittedUnit, only int) error {
	name := w.c.Name()
	bury := func(item int, reason string) error {
		w.dead.Add(1, name)
		w.d.Log.Error("dispatcher: dead letter", "consumer", name, "position", uint64(u.Position), "item", item,
			"tenant", string(u.Tenant), "reason", reason)
		return w.d.Coord.Bury(ctx, port.DeadLetter{Consumer: name, Position: u.Position, Item: item, Tenant: u.Tenant,
			Reason: reason, At: w.d.Now()})
	}
	err := w.c.Deliver(ctx, u, only, bury)
	if errors.Is(err, port.ErrPermanent) {
		return bury(-1, err.Error())
	}
	return err
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// Retry retries the delivery of a consumer's dead letters ("" — of all): a
// letter delivered successfully is deleted. It returns the number delivered.
func (d *Dispatcher) Retry(ctx context.Context, consumer string) (int, error) {
	d.defaults()
	letters, err := d.Coord.DeadLetters(ctx, consumer)
	if err != nil {
		return 0, err
	}
	byName := map[string]Consumer{}
	for _, c := range d.Consumers {
		byName[c.Name()] = c
	}
	n := 0
	for _, dl := range letters {
		c := byName[dl.Consumer]
		if c == nil {
			continue
		}
		units, err := d.Effects.Read(ctx, dl.Position-1, 1)
		if err != nil {
			return n, err
		}
		if len(units) == 0 || units[0].Position != dl.Position {
			return n, fmt.Errorf("dispatch: unit of work %d is not in the outbox", dl.Position)
		}
		if err := d.Coord.Resurrect(ctx, dl); err != nil {
			return n, err
		}
		w := &worker{d: d, c: c, delivered: ports.Nop{}, failed: ports.Nop{}, dead: ports.Nop{}, lag: ports.Nop{}}
		if err := w.deliver(ctx, &units[0], dl.Item); err != nil {
			_ = d.Coord.Bury(ctx, dl) // a fault — the letter stays
			return n, err
		}
		if still, _ := d.Coord.DeadLetters(ctx, dl.Consumer); !contains(still, dl) {
			n++
		}
	}
	return n, nil
}

func contains(list []port.DeadLetter, d port.DeadLetter) bool {
	for _, x := range list {
		if x.Position == d.Position && x.Item == d.Item {
			return true
		}
	}
	return false
}
