package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Long polling: the server keeps a subscription and a short buffer of the
// latest notifications per actor — a delivery buffer, not storage (RFC
// §13). The first poll creates the subscription, and it closes if the
// actor has not polled the server for PollIdle.
const (
	PollBuffer  = 100              // notifications in the buffer
	PollMaxAge  = 60 * time.Second // how long a notification lives in the buffer
	PollIdle    = 60 * time.Second // how long a subscription lives without polls
	PollWait    = 30 * time.Second // the default wait
	PollMaxWait = 60 * time.Second
)

type polls struct {
	s      *Server
	mu     sync.Mutex
	m      map[pollKey]*poller
	closed bool
}

type pollKey struct {
	tenant port.Tenant
	actor  string
}

type poller struct {
	mu      sync.Mutex
	entries []pollEntry
	seq     uint64
	last    time.Time
	wake    chan struct{} // closes on a new notification
	cancel  context.CancelFunc
}

type pollEntry struct {
	seq  uint64
	at   time.Time
	json []byte
}

func newPolls(s *Server) *polls { return &polls{s: s, m: map[pollKey]*poller{}} }

// get returns the actor's buffer, subscribing on the first poll.
func (ps *polls) get(tenant port.Tenant, sc *reasonr.Schema, a *reasonr.Arena, actor reasonr.Value) (*poller, error) {
	key := pollKey{tenant, a.Format(actor)}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if p := ps.m[key]; p != nil {
		return p, nil
	}
	if ps.closed {
		return nil, context.Canceled
	}
	ctx, cancel := context.WithCancel(context.Background())
	sub, err := ps.s.x.Subscribe(ctx, tenant, a, actor)
	if err != nil {
		cancel()
		return nil, err
	}
	p := &poller{last: time.Now(), wake: make(chan struct{}), cancel: cancel}
	ps.m[key] = p
	go ps.pump(key, p, sub, sc)
	return p, nil
}

// pump moves the subscription's notifications to the buffer and closes the
// subscription after PollIdle without polls.
func (ps *polls) pump(key pollKey, p *poller, sub kind.Subscription, sc *reasonr.Schema) {
	defer sub.Close()
	idle := time.NewTicker(PollIdle / 4)
	defer idle.Stop()
	for {
		select {
		case d, ok := <-sub.C():
			if !ok {
				ps.drop(key, p)
				return
			}
			b, err := ps.s.notification(nil, sc, d.Arena, d.Notification)
			if err != nil {
				ps.s.log.Error("the notification cannot be encoded", "error", err)
				continue
			}
			p.add(b)
		case <-idle.C:
			p.mu.Lock()
			stale := time.Since(p.last) > PollIdle
			p.mu.Unlock()
			if stale {
				ps.drop(key, p)
				return
			}
		}
	}
}

func (ps *polls) drop(key pollKey, p *poller) {
	ps.mu.Lock()
	if ps.m[key] == p {
		delete(ps.m, key)
	}
	ps.mu.Unlock()
	p.cancel()
}

// close closes every subscription (server shutdown).
func (ps *polls) close() {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.closed = true
	for k, p := range ps.m {
		p.cancel()
		delete(ps.m, k)
	}
}

func (p *poller) add(b []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	now := time.Now()
	p.entries = append(p.entries, pollEntry{seq: p.seq, at: now, json: b})
	p.trim(now)
	close(p.wake)
	p.wake = make(chan struct{})
}

// trim removes old notifications and the ones over PollBuffer.
func (p *poller) trim(now time.Time) {
	i := 0
	for i < len(p.entries) && (len(p.entries)-i > PollBuffer || now.Sub(p.entries[i].at) > PollMaxAge) {
		i++
	}
	p.entries = append(p.entries[:0], p.entries[i:]...)
}

// after returns the notifications after cursor, the new cursor and the
// wake channel.
func (p *poller) after(cursor uint64) ([][]byte, uint64, <-chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last = time.Now()
	p.trim(p.last)
	if cursor > p.seq {
		cursor = 0 // the cursor of a previous subscription: give the whole buffer
	}
	var out [][]byte
	for _, e := range p.entries {
		if e.seq > cursor {
			out = append(out, e.json)
		}
	}
	return out, p.seq, p.wake
}

// poll — GET /api/poll?cursor=…&wait=30s.
func (s *Server) poll(w http.ResponseWriter, r *http.Request, c *call) {
	a := reasonr.NewArena()
	tenant, caller, sc, ok := s.authenticate(w, r, c, a, true)
	if !ok {
		return
	}
	if caller.Subject.Kind == reasonr.KindNone {
		http.Error(w, "only actors get notifications", http.StatusForbidden)
		return
	}
	q := r.URL.Query()
	var cursor uint64
	if v := q.Get("cursor"); v != "" {
		var err error
		if cursor, err = strconv.ParseUint(v, 10, 64); err != nil {
			invalid(w, "cursor", "type")
			return
		}
	}
	wait := PollWait
	if v := q.Get("wait"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			invalid(w, "wait", "type")
			return
		}
		wait = min(d, PollMaxWait)
	}
	p, err := s.polls.get(tenant, sc, a, caller.Subject)
	if err != nil {
		s.fail(w, r, c, err)
		return
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		out, next, wake := p.after(cursor)
		if len(out) > 0 || wait == 0 {
			b := append([]byte(`{"cursor":"`), strconv.FormatUint(next, 10)...)
			b = append(b, `","notifications":[`...)
			for i, n := range out {
				if i > 0 {
					b = append(b, ',')
				}
				b = append(b, n...)
			}
			writeJSON(w, http.StatusOK, append(b, "]}"...))
			return
		}
		select {
		case <-wake:
		case <-timer.C:
			wait = 0
		case <-r.Context().Done():
			return
		}
	}
}
