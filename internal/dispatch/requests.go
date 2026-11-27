package dispatch

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/http/integration"
	"github.com/tycheg/reasonr-engine/internal/bus"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// RetryPolicy is the retry strategy of external requests
// (integrations.retry, RFC §18): the pause before attempt n+1 is Initial ×
// Multiplier^(n-1), at most MaxInterval. A request without a deadline
// timer is retried until MaxElapsed has passed since the time of its unit
// of work, then dropped; the timer sets the deadline of a request with one.
type RetryPolicy struct {
	Initial     time.Duration // 0 — 1 s
	Multiplier  float64       // 0 — 2
	MaxInterval time.Duration // 0 — 1 min
	MaxElapsed  time.Duration // 0 — 1 h
}

// DefaultRetry is the default strategy.
var DefaultRetry = RetryPolicy{Initial: time.Second, Multiplier: 2, MaxInterval: time.Minute, MaxElapsed: time.Hour}

// withDefaults fills zero fields with the values of DefaultRetry.
func (p RetryPolicy) withDefaults() RetryPolicy {
	if p.Initial <= 0 {
		p.Initial = DefaultRetry.Initial
	}
	if p.Multiplier <= 0 {
		p.Multiplier = DefaultRetry.Multiplier
	}
	if p.MaxInterval <= 0 {
		p.MaxInterval = DefaultRetry.MaxInterval
	}
	if p.MaxElapsed <= 0 {
		p.MaxElapsed = DefaultRetry.MaxElapsed
	}
	return p
}

// Pause is the pause after the failed attempt attempt (from 1).
func (p RetryPolicy) Pause(attempt int) time.Duration {
	d := float64(p.Initial)
	for i := 1; i < attempt && d < float64(p.MaxInterval); i++ {
		d *= p.Multiplier
	}
	return min(time.Duration(d), p.MaxInterval)
}

// Sender sends external requests to the integration service (RFC §18): the
// store writes requests at commit, and the sender leases the ready requests
// of every tenant and system — up to Workers at once — and sends them in
// parallel, each with its own attempts, so an unavailable system delays
// only its own requests. The engine's job is to make the request leave:
// the system sends its answer as an API command. The attempt outcomes:
//
//   - the deadline timer fired or was deleted — the request is no longer
//     needed (expired): the model reacts to the missing answer with the
//     timer command;
//   - the subject of a non-optional personal argument is forgotten — the
//     request is not sent (forgotten, RFC §20.3);
//   - 2xx — delivered (delivered);
//   - an error — retried by Retry (retry); a request without a deadline
//     timer is dropped after Retry.MaxElapsed from its unit of work (dropped);
//   - the system has no transport or the request cannot be encoded
//     (port.ErrPermanent) — dropped (dropped).
type Sender struct {
	Queue   kind.RequestQueue
	Default ports.Transport            // nil — only the systems from Systems
	Systems map[string]ports.Transport // by external system name
	Catalog kind.Catalog
	// Clock is the engine clock: the Retry.MaxElapsed deadline and the
	// delivery delay count by it from the time of the unit of work; nil — the system clock.
	Clock port.Clock
	// Now is the sender clock: leases and retry pauses; nil — time.Now.
	Now     func() time.Time
	Workers int // concurrent sends; 0 — 64
	Retry   RetryPolicy
	// LeaseTTL is the lease of a request for an attempt: longer than the
	// transport timeout; 0 — a minute.
	LeaseTTL time.Duration
	// Poll is how often to look for requests when none are ready; 0 — 500 ms.
	Poll    time.Duration
	Log     *slog.Logger
	Metrics port.Metrics

	once    sync.Once
	wake    chan struct{}
	total   port.Counter
	latency port.Histogram
}

func (s *Sender) init() {
	s.once.Do(func() {
		if s.Workers <= 0 {
			s.Workers = 64
		}
		s.Retry = s.Retry.withDefaults()
		if s.LeaseTTL == 0 {
			s.LeaseTTL = time.Minute
		}
		if s.Poll == 0 {
			s.Poll = 500 * time.Millisecond
		}
		if s.Clock == nil {
			s.Clock = ports.SystemClock{}
		}
		if s.Now == nil {
			s.Now = time.Now
		}
		if s.Log == nil {
			s.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
		}
		if s.Metrics == nil {
			s.Metrics = ports.Nop{}
		}
		s.wake = make(chan struct{}, 1)
		s.total = s.Metrics.Counter("reasonr_requests_total",
			"External request delivery attempt outcomes: delivered, retry, expired, forgotten, dropped.", "system", "outcome")
		s.latency = s.Metrics.Histogram("reasonr_request_delivery_seconds", "Time from committing the unit of work to delivering the request.",
			[]float64{.01, .05, .1, .2, .5, 1, 5, 30, 60, 300, 3600}, "system")
	})
}

// Wake reports that there are new requests: the sender looks for them at
// once, without waiting for Poll.
func (s *Sender) Wake() {
	s.init()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// Run sends requests until ctx is cancelled and waits for the started attempts.
func (s *Sender) Run(ctx context.Context) error {
	s.init()
	slots := make(chan struct{}, s.Workers)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		select { // at least one free send
		case slots <- struct{}{}:
		case <-ctx.Done():
			return nil
		}
		n := 1
	more:
		for n < s.Workers {
			select {
			case slots <- struct{}{}:
				n++
			default:
				break more
			}
		}
		rs, err := s.Queue.LeaseRequests(ctx, s.Now(), n, s.LeaseTTL)
		if err != nil && ctx.Err() == nil {
			s.Log.Warn("external requests: the lease is not acquired", "error", err)
		}
		for range n - len(rs) {
			<-slots
		}
		for i := range rs {
			wg.Add(1)
			go func(r *kind.LeasedRequest) {
				defer wg.Done()
				defer func() { <-slots }()
				s.deliver(ctx, r)
			}(&rs[i])
		}
		if len(rs) == 0 {
			t := time.NewTimer(s.Poll)
			select {
			case <-ctx.Done():
				t.Stop()
				return nil
			case <-s.wake:
			case <-t.C:
			}
			t.Stop()
		}
	}
}

// SendDue makes an attempt for every request ready by Now() — up to
// Workers at once — and returns their number. A request postponed for a
// retry is not selected again in this pass.
func (s *Sender) SendDue(ctx context.Context) (int, error) {
	s.init()
	n := 0
	for ctx.Err() == nil {
		rs, err := s.Queue.LeaseRequests(ctx, s.Now(), s.Workers, s.LeaseTTL)
		if err != nil || len(rs) == 0 {
			return n, err
		}
		var wg sync.WaitGroup
		for i := range rs {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.deliver(ctx, &rs[i])
			}()
		}
		wg.Wait()
		n += len(rs)
	}
	return n, ctx.Err()
}

// deliver makes one delivery attempt of a leased request.
func (s *Sender) deliver(ctx context.Context, r *kind.LeasedRequest) {
	sch, err := s.Catalog.Schema(r.Tenant)
	if err != nil {
		s.retry(ctx, r, "?", err)
		return
	}
	ti := sch.Type(r.Request.System)
	if ti == nil || int(r.Request.Request) >= len(ti.Methods) {
		s.drop(ctx, r, "?", fmt.Sprintf("the tenant's model has no external system %d", r.Request.System))
		return
	}
	mi := &ti.Methods[r.Request.Request]
	if r.TimerGone {
		s.done(ctx, r, ti.Name, "expired")
		return
	}
	if param := forgottenParam(mi.Params, r.Arena, r.Request.Args); param != "" {
		s.Log.Warn("external request not sent: the subject of a personal argument is forgotten", "tenant", string(r.Tenant),
			"position", uint64(r.Position), "request", ti.Name+"."+mi.Name, "param", param)
		s.done(ctx, r, ti.Name, "forgotten")
		return
	}
	t := s.Systems[ti.Short()]
	if t == nil {
		t = s.Default
	}
	if t == nil { // reasonrd check warns about it at startup (C0004)
		s.Log.Debug("the external system has no transport (integrations): the request is dropped", "tenant", string(r.Tenant), "system", ti.Name)
		s.done(ctx, r, ti.Name, "dropped")
		return
	}
	if err := ready(sch, ti, mi, t, r); err != nil {
		s.drop(ctx, r, ti.Name, err.Error())
		return
	}
	err = t.Send(ctx, r)
	switch {
	case ctx.Err() != nil:
		// Stopping: the lease expires, and another instance sends the request.
	case err == nil:
		s.latency.Observe(s.Clock.Now(r.Tenant).Sub(r.At).Seconds(), ti.Name)
		s.done(ctx, r, ti.Name, "delivered")
	case errors.Is(err, port.ErrPermanent):
		s.drop(ctx, r, ti.Name, err.Error())
	case r.Deadline.IsZero() && s.Clock.Now(r.Tenant).Sub(r.At) >= s.Retry.MaxElapsed:
		s.drop(ctx, r, ti.Name, fmt.Sprintf("retries are exhausted (integrations.retry.max_elapsed %s): %v", s.Retry.MaxElapsed, err))
	default:
		s.retry(ctx, r, ti.Name, err)
	}
}

func (s *Sender) done(ctx context.Context, r *kind.LeasedRequest, system, outcome string) {
	s.total.Add(1, system, outcome)
	if err := s.Queue.RequestDone(ctx, r); err != nil {
		s.Log.Warn("external requests: the request is not deleted", "tenant", string(r.Tenant), "error", err)
	}
}

func (s *Sender) retry(ctx context.Context, r *kind.LeasedRequest, system string, cause error) {
	s.total.Add(1, system, "retry")
	at := s.Now().Add(s.Retry.Pause(r.Attempts))
	s.Log.Warn("external request not delivered, retrying", "tenant", string(r.Tenant), "position", uint64(r.Position),
		"system", system, "attempt", r.Attempts, "retry", at, "error", cause)
	if err := s.Queue.RetryRequest(ctx, r, at, cause.Error()); err != nil {
		s.Log.Warn("external requests: the retry is not written", "tenant", string(r.Tenant), "error", err)
	}
}

// drop drops a request that is no longer sent.
func (s *Sender) drop(ctx context.Context, r *kind.LeasedRequest, system, reason string) {
	s.Log.Warn("external request dropped", "tenant", string(r.Tenant), "position", uint64(r.Position), "system", system,
		"attempt", r.Attempts, "reason", reason)
	s.done(ctx, r, system, "dropped")
}

// forgottenParam returns the name of a non-optional personal parameter
// whose argument is none: its subject was forgotten after commit, and the
// store did not decrypt the value. "" — none.
func forgottenParam(params []reasonr.FieldInfo, a *reasonr.Arena, args reasonr.Record) string {
	vals := a.Elems(args.Value())
	for i := range params {
		if params[i].Personal != nil && !params[i].Type.Optional && i < len(vals) && vals[i].Kind == reasonr.KindNone {
			return params[i].Name
		}
	}
	return ""
}

// ReadyRequest fills what the transport t gets ready in the request r, as
// the sender does before Send (tests of transports).
func ReadyRequest(c kind.Catalog, t ports.Transport, r *kind.LeasedRequest) error {
	s, err := c.Schema(r.Tenant)
	if err != nil {
		return err
	}
	ti := s.Type(r.Request.System)
	if ti == nil || int(r.Request.Request) >= len(ti.Methods) {
		return fmt.Errorf("unknown request %d.%d", r.Request.System, r.Request.Request)
	}
	return ready(s, ti, &ti.Methods[r.Request.Request], t, r)
}

// ready fills what a transport gets ready (port.LeasedRequest): the names,
// the idempotency key, the body, the CloudEvents attributes and the topic.
func ready(s *reasonr.Schema, ti *reasonr.TypeInfo, mi *reasonr.MethodInfo, t ports.Transport, r *kind.LeasedRequest) error {
	body, err := integration.Body(s, r.Arena, mi, r.Request.Args, nil)
	if err != nil {
		return fmt.Errorf("arguments of %s.%s: %v", ti.Name, mi.Name, err)
	}
	r.System, r.Name, r.Key, r.Body = ti.Short(), mi.Name, integration.Key(r.Arena, r.Request.Key), body
	r.Attributes = bus.RequestAttributes(r)
	if tb, ok := ports.TopicBroker(t); ok {
		r.Topic = bus.RequestTopic(cmp.Or(tb.TopicTemplate(), bus.DefaultRequestTopic), r.Tenant, r.System)
	}
	return nil
}
