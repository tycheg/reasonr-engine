package memory

import (
	"context"
	"sync"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Hub is an in-memory notification hub of one process
// (port.NotificationHub). A notification goes to the recipient's
// subscribers at most once: if a subscription buffer is full, it is lost
// (RFC §13). With a journal the hub also remembers everything passed — for
// the test driver.
type Hub struct {
	mu      sync.Mutex
	subs    map[port.Tenant]map[uint64][]*subscription // by the recipient hash
	journal bool
	log     *reasonr.Arena
	sent    []Sent
	forgets []func(port.Tenant, []port.SubjectRef)
	flags   []func() // subscribers to feature flag rollout changes
}

var (
	_ kind.NotificationHub = (*Hub)(nil)
	_ ports.ForgetHub      = (*Hub)(nil)
)

// Sent is a notification passed to the hub; the values are in the journal arena.
type Sent struct {
	Tenant       port.Tenant
	Notification reasonr.Notification
}

// SubscriptionBuffer is how many notifications wait for a subscriber to read them.
const SubscriptionBuffer = 64

// NewHub returns a hub; journal — remember the passed notifications.
func NewHub(journal bool) *Hub {
	return &Hub{subs: map[port.Tenant]map[uint64][]*subscription{}, journal: journal, log: reasonr.NewArena()}
}

// Publish passes notifications to the subscribers of their recipients.
func (h *Hub) Publish(_ context.Context, tenant port.Tenant, a *reasonr.Arena, ns []reasonr.Notification) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, n := range ns {
		if n.To.Kind == reasonr.KindNone {
			continue
		}
		if h.journal {
			h.sent = append(h.sent, Sent{Tenant: tenant, Notification: copyNotification(h.log, a, n)})
		}
		for _, sub := range h.subs[tenant][a.Hash(n.To)] {
			if !reasonr.Equal(sub.a, sub.actor, a, n.To) {
				continue
			}
			da := reasonr.NewArena()
			select {
			case sub.ch <- kind.Delivery{Notification: copyNotification(da, a, n), Arena: da}:
			default: // the subscriber cannot keep up: the notification is not delivered
			}
		}
	}
	return nil
}

func copyNotification(dst, src *reasonr.Arena, n reasonr.Notification) reasonr.Notification {
	return reasonr.Notification{Owner: n.Owner, Name: n.Name, Args: dst.Copy(src, n.Args.Value()).Record(), To: dst.Copy(src, n.To)}
}

// Journal returns the journal arena and what was passed from the number from.
func (h *Hub) Journal(from int) (*reasonr.Arena, []Sent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	from = min(from, len(h.sent))
	return h.log, append([]Sent(nil), h.sent[from:]...)
}

// Subscribe subscribes to an actor's notifications; the subscription
// closes with Close or when ctx is cancelled.
func (h *Hub) Subscribe(ctx context.Context, tenant port.Tenant, a *reasonr.Arena, actor reasonr.Value) (kind.Subscription, error) {
	sa := reasonr.NewArena()
	sub := &subscription{h: h, tenant: tenant, a: sa, actor: sa.Copy(a, actor), hash: a.Hash(actor),
		ch: make(chan kind.Delivery, SubscriptionBuffer)}
	h.mu.Lock()
	m := h.subs[tenant]
	if m == nil {
		m = map[uint64][]*subscription{}
		h.subs[tenant] = m
	}
	m[sub.hash] = append(m[sub.hash], sub)
	h.mu.Unlock()
	sub.stop = context.AfterFunc(ctx, func() { _ = sub.Close() })
	return sub, nil
}

type subscription struct {
	h      *Hub
	tenant port.Tenant
	a      *reasonr.Arena
	actor  reasonr.Value
	hash   uint64
	ch     chan kind.Delivery
	stop   func() bool
	closed bool
}

func (s *subscription) C() <-chan kind.Delivery { return s.ch }

func (s *subscription) Close() error {
	h := s.h
	h.mu.Lock()
	defer h.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if s.stop != nil {
		s.stop()
	}
	list := h.subs[s.tenant][s.hash]
	for i, x := range list {
		if x == s {
			h.subs[s.tenant][s.hash] = append(list[:i:i], list[i+1:]...)
			break
		}
	}
	close(s.ch)
	return nil
}

// PublishForgotten passes forgotten subjects to the subscribers (ports.ForgetHub).
func (h *Hub) PublishForgotten(_ context.Context, tenant port.Tenant, subjects []port.SubjectRef) error {
	h.mu.Lock()
	fns := h.forgets
	h.mu.Unlock()
	for _, fn := range fns {
		fn(tenant, subjects)
	}
	return nil
}

// OnForgotten subscribes fn to forgotten subjects (ports.ForgetHub).
func (h *Hub) OnForgotten(fn func(port.Tenant, []port.SubjectRef)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.forgets = append(h.forgets, fn)
}
