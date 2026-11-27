package dispatch

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/bus"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// EventsConsumer is the name of the outbox consumer that writes the event
// store (Events): its cursor is the bus's read boundary.
const EventsConsumer = "events"

// BusCursor is the bus cursor in the coordinator. The positions are the
// same as the outbox's, so the cursor of the previous bus consumer
// (publishing from the outbox) continues without publishing again.
const BusCursor = "bus"

// Bus publishes events to the bus from the event store (RFC §31): every
// event in position order, at least once, ordered within an entity. It
// reads only the positions the events consumer has already written (its
// cursor) — so the bus does not see a half-written unit of work. One
// instance runs the stream (lease dispatch:bus); on a bus fault the batch
// repeats from the cursor, and events are neither lost nor reordered.
type Bus struct {
	Feed      port.EventFeed
	Publisher port.Publisher
	Coord     port.Coordinator
	// Holder is the instance name in the lease; "" — the host, the process
	// and a random suffix.
	Holder   string
	LeaseTTL time.Duration // 0 — 30 s
	Batch    int           // events per batch; 0 — 500
	// Poll is how often to check for new events without a Wake signal; 0 —
	// 500 ms.
	Poll    time.Duration
	Log     *slog.Logger
	Metrics port.Metrics
	// Pause is the pause before a retry after a fault; nil — RetryPause.
	Pause func(attempt int) time.Duration

	once      sync.Once
	wake      chan struct{}
	published port.Counter
	seconds   port.Histogram
	failures  port.Counter
}

func (b *Bus) init() {
	b.once.Do(func() {
		if b.LeaseTTL == 0 {
			b.LeaseTTL = 30 * time.Second
		}
		if b.Batch == 0 {
			b.Batch = 500
		}
		if b.Poll == 0 {
			b.Poll = 500 * time.Millisecond
		}
		if b.Log == nil {
			b.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
		}
		if b.Metrics == nil {
			b.Metrics = ports.Nop{}
		}
		if b.Pause == nil {
			b.Pause = RetryPause
		}
		if b.Holder == "" {
			b.Holder = holder()
		}
		b.wake = make(chan struct{}, 1)
		b.published = b.Metrics.Counter("reasonr_bus_published_total", "Messages published to the bus.", "channel")
		b.seconds = b.Metrics.Histogram("reasonr_bus_publish_seconds", "Time to publish a batch to the bus.",
			[]float64{.001, .005, .01, .05, .1, .5, 1, 5, 30}, "kind")
		b.failures = b.Metrics.Counter("reasonr_bus_failures_total", "Event publishing faults (to be retried).")
	})
}

// Wake reports that the events consumer wrote new events.
func (b *Bus) Wake() {
	b.init()
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// Run publishes events until ctx is cancelled.
func (b *Bus) Run(ctx context.Context) error {
	b.init()
	renew := b.LeaseTTL / 3
	attempt := 0
	defer func() { release(b.Coord, "dispatch:"+BusCursor, b.Holder, time.Now()) }()
	for ctx.Err() == nil {
		ok, err := b.Coord.Acquire(ctx, "dispatch:"+BusCursor, b.Holder, time.Now(), b.LeaseTTL)
		if err != nil || !ok {
			if err != nil {
				b.Log.Warn("bus: the lease is not acquired", "error", err)
			}
			sleep(ctx, renew)
			continue
		}
		for deadline := time.Now().Add(renew); ctx.Err() == nil && time.Now().Before(deadline); {
			_, moved, err := b.publish(ctx)
			switch {
			case err != nil && ctx.Err() == nil:
				b.failures.Add(1)
				level := slog.LevelWarn
				if errors.Is(err, port.ErrPermanent) {
					level = slog.LevelError
				}
				attempt++
				b.Log.Log(ctx, level, "bus: events not published, retrying", "attempt", attempt, "error", err)
				sleep(ctx, b.Pause(attempt-1))
			case !moved:
				attempt = 0
				t := time.NewTimer(b.Poll)
				select {
				case <-ctx.Done():
				case <-b.wake:
				case <-t.C:
				}
				t.Stop()
			default:
				attempt = 0
			}
		}
	}
	return nil
}

// PublishDue publishes every event written by now, without a lease (tests,
// like FireDue for timers), and returns their number.
func (b *Bus) PublishDue(ctx context.Context) (int, error) {
	b.init()
	total := 0
	for {
		n, moved, err := b.publish(ctx)
		total += n
		if err != nil || !moved {
			return total, err
		}
	}
}

// publish publishes one batch after the cursor and moves it: n — the
// number published, moved — the cursor moved (positions without events
// move it too).
func (b *Bus) publish(ctx context.Context) (n int, moved bool, err error) {
	cursor, err := b.Coord.Cursor(ctx, BusCursor)
	if err != nil {
		return 0, false, err
	}
	upto, err := b.Coord.Cursor(ctx, EventsConsumer)
	if err != nil || upto <= cursor {
		return 0, false, err
	}
	evs, err := b.Feed.ReadEvents(ctx, cursor, upto, b.Batch)
	if err != nil {
		return 0, false, err
	}
	next := upto
	if len(evs) == b.Batch { // the batch may have cut the last unit
		last := evs[len(evs)-1].Position
		if evs[0].Position == last {
			if evs, err = b.Feed.ReadEvents(ctx, last-1, last, 0); err != nil {
				return 0, false, err
			}
		} else {
			for evs[len(evs)-1].Position == last {
				evs = evs[:len(evs)-1]
			}
			last = evs[len(evs)-1].Position
		}
		next = last
	}
	msgs := make([]port.Message, len(evs))
	for i := range evs {
		msgs[i] = bus.FromEvent(&evs[i])
	}
	if len(msgs) > 0 {
		start := time.Now()
		if err := b.Publisher.Publish(ctx, msgs); err != nil {
			return 0, false, err
		}
		b.seconds.Observe(time.Since(start).Seconds(), port.MessageEvent.String())
		for i := range msgs {
			b.published.Add(1, msgs[i].Channel())
		}
	}
	if err := b.Coord.SetCursor(ctx, BusCursor, next); err != nil {
		return 0, false, err
	}
	return len(msgs), true, nil
}

// Notifier publishes notifications to the bus without guarantees (RFC
// §13): after commit the kernel passes them to Notify, they are encoded and
// wait in the process queue; Run publishes them in batches. If the queue
// is full or the bus answers with an error, the notifications are lost
// (reasonr_bus_dropped_total).
type Notifier struct {
	Publisher port.Publisher
	Catalog   kind.Catalog
	Buffer    int // notifications in the queue; 0 — 10000
	Batch     int // per batch; 0 — 500
	Log       *slog.Logger
	Metrics   port.Metrics

	once      sync.Once
	queue     chan port.Message
	published port.Counter
	dropped   port.Counter
	seconds   port.Histogram
}

func (n *Notifier) init() {
	n.once.Do(func() {
		if n.Buffer == 0 {
			n.Buffer = 10000
		}
		if n.Batch == 0 {
			n.Batch = 500
		}
		if n.Log == nil {
			n.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
		}
		if n.Metrics == nil {
			n.Metrics = ports.Nop{}
		}
		n.queue = make(chan port.Message, n.Buffer)
		n.published = n.Metrics.Counter("reasonr_bus_published_total", "Messages published to the bus.", "channel")
		n.dropped = n.Metrics.Counter("reasonr_bus_dropped_total", "Notifications not published to the bus (no guarantees).")
		n.seconds = n.Metrics.Histogram("reasonr_bus_publish_seconds", "Time to publish a batch to the bus.",
			[]float64{.001, .005, .01, .05, .1, .5, 1, 5, 30}, "kind")
	})
}

// Notify queues the notifications of a committed unit of work
// (kernel.Config.Notify): it encodes them at once — the arena may be freed
// after it returns — and does not wait for the bus.
func (n *Notifier) Notify(tenant port.Tenant, pos port.Position, at time.Time, a *reasonr.Arena, ns []reasonr.Notification) {
	n.init()
	s, err := n.Catalog.Schema(tenant)
	var msgs []port.Message
	if err == nil {
		msgs, err = bus.NewEncoder(s).Notifications(tenant, pos, at, a, ns)
	}
	if err != nil {
		n.dropped.Add(float64(len(ns)))
		n.Log.Warn("bus: notifications not encoded", "tenant", string(tenant), "position", uint64(pos), "error", err)
		return
	}
	for _, m := range msgs {
		select {
		case n.queue <- m:
		default:
			n.dropped.Add(1)
		}
	}
}

// Run publishes notifications from the queue until ctx is cancelled.
func (n *Notifier) Run(ctx context.Context) error {
	n.init()
	for {
		select {
		case <-ctx.Done():
			return nil
		case m := <-n.queue:
			n.send(ctx, m)
		}
	}
}

// Flush publishes the notifications already in the queue (tests).
func (n *Notifier) Flush(ctx context.Context) {
	n.init()
	for {
		select {
		case m := <-n.queue:
			n.send(ctx, m)
		default:
			return
		}
	}
}

// send publishes m and everything already waiting in the queue, up to Batch.
func (n *Notifier) send(ctx context.Context, m port.Message) {
	msgs := []port.Message{m}
more:
	for len(msgs) < n.Batch {
		select {
		case m := <-n.queue:
			msgs = append(msgs, m)
		default:
			break more
		}
	}
	start := time.Now()
	if err := n.Publisher.Publish(ctx, msgs); err != nil {
		n.dropped.Add(float64(len(msgs)))
		if ctx.Err() == nil {
			n.Log.Warn("bus: notifications not published", "count", len(msgs), "error", err)
		}
		return
	}
	n.seconds.Observe(time.Since(start).Seconds(), port.MessageNotification.String())
	for i := range msgs {
		n.published.Add(1, msgs[i].Channel())
	}
}
