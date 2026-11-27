// Package hub is the notification hub of several server instances over a
// link (port.HubLink): notifications to other actors, forgotten subjects
// and rollout changes go to every instance as bytes, each instance hands
// them to its own subscribers (memory.Hub).
package hub

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/memory"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/internal/storekit"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Hub is a notification hub over a link.
type Hub struct {
	link  port.HubLink
	local *memory.Hub
	log   *slog.Logger

	mu      sync.Mutex
	started bool
}

var (
	_ kind.NotificationHub = (*Hub)(nil)
	_ ports.ForgetHub      = (*Hub)(nil)
	_ ports.FeatureHub     = (*Hub)(nil)
	_ ports.Listening      = (*Hub)(nil)
)

// The first byte of a message on the link.
const (
	kindNote      = 'n'
	kindForgotten = 'f'
	kindFeatures  = 'g'
)

// New returns a hub over the link.
func New(link port.HubLink, log *slog.Logger) *Hub {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Hub{link: link, local: memory.NewHub(false), log: log}
}

// start starts receiving from the link once.
func (h *Hub) start(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.started {
		return nil
	}
	if err := h.link.Listen(ctx, h.deliver); err != nil {
		return err
	}
	h.started = true
	return nil
}

// deliver hands a message of the link to the subscribers of this instance.
func (h *Hub) deliver(msg []byte) {
	if err := h.receive(msg); err != nil {
		h.log.Error("notification hub: a message cannot be read", "error", err)
	}
}

func (h *Hub) receive(msg []byte) error {
	if len(msg) == 0 {
		return errNote
	}
	ctx := context.Background()
	switch msg[0] {
	case kindNote:
		a := reasonr.NewArena()
		tenant, n, err := decodeNote(a, msg[1:])
		if err != nil {
			return err
		}
		return h.local.Publish(ctx, tenant, a, []reasonr.Notification{n})
	case kindForgotten:
		var m forgottenMsg
		if err := json.Unmarshal(msg[1:], &m); err != nil {
			return err
		}
		return h.local.PublishForgotten(ctx, m.Tenant, m.Subjects)
	case kindFeatures:
		return h.local.PublishFeatures(ctx)
	}
	return errNote
}

// Publish sends notifications to the instances (port.NotificationHub).
func (h *Hub) Publish(ctx context.Context, tenant port.Tenant, a *reasonr.Arena, ns []reasonr.Notification) error {
	if err := h.start(ctx); err != nil {
		return err
	}
	for _, n := range ns {
		if n.To.Kind == reasonr.KindNone {
			continue
		}
		if err := h.link.Send(ctx, encodeNote(tenant, a, n)); err != nil {
			return err
		}
	}
	return nil
}

// Subscribe subscribes to an actor's notifications (port.NotificationHub).
func (h *Hub) Subscribe(ctx context.Context, tenant port.Tenant, a *reasonr.Arena, actor reasonr.Value) (kind.Subscription, error) {
	if err := h.start(ctx); err != nil {
		return nil, err
	}
	return h.local.Subscribe(ctx, tenant, a, actor)
}

// forgottenMsg is the message about forgotten subjects.
type forgottenMsg struct {
	Tenant   port.Tenant       `json:"t"`
	Subjects []port.SubjectRef `json:"s"`
}

// PublishForgotten tells the instances about forgotten subjects (ports.ForgetHub).
func (h *Hub) PublishForgotten(ctx context.Context, tenant port.Tenant, subjects []port.SubjectRef) error {
	if err := h.start(ctx); err != nil {
		return err
	}
	b, err := json.Marshal(forgottenMsg{Tenant: tenant, Subjects: subjects})
	if err != nil {
		return err
	}
	return h.link.Send(ctx, append([]byte{kindForgotten}, b...))
}

// OnForgotten subscribes fn to forgotten subjects (ports.ForgetHub).
func (h *Hub) OnForgotten(fn func(port.Tenant, []port.SubjectRef)) { h.local.OnForgotten(fn) }

// PublishFeatures tells the instances that the rollout changed (ports.FeatureHub).
func (h *Hub) PublishFeatures(ctx context.Context) error {
	if err := h.start(ctx); err != nil {
		return err
	}
	return h.link.Send(ctx, []byte{kindFeatures})
}

// OnFeatures subscribes fn to rollout changes (ports.FeatureHub).
func (h *Hub) OnFeatures(fn func()) { h.local.OnFeatures(fn) }

// Listen starts receiving (ports.Listening): without it an instance learns
// about rollout changes only when it rereads the rules.
func (h *Hub) Listen(ctx context.Context) error { return h.start(ctx) }

// Close stops the link.
func (h *Hub) Close() error { return h.link.Close() }

// encodeNote encodes the tenant, the owner, the method, the arguments and
// the recipient without a schema (the instances run one model version).
func encodeNote(tenant port.Tenant, a *reasonr.Arena, n reasonr.Notification) []byte {
	b := []byte{kindNote}
	b = binary.AppendUvarint(b, uint64(len(tenant)))
	b = append(b, tenant...)
	b = binary.AppendUvarint(b, uint64(n.Owner))
	b = binary.AppendUvarint(b, uint64(n.Name))
	args := storekit.AppendValue(nil, a, n.Args.Value())
	b = binary.AppendUvarint(b, uint64(len(args)))
	b = append(b, args...)
	return storekit.AppendValue(b, a, n.To)
}

var errNote = errors.New("notification hub: a corrupt message")

func decodeNote(a *reasonr.Arena, b []byte) (port.Tenant, reasonr.Notification, error) {
	var n reasonr.Notification
	l, k := binary.Uvarint(b)
	if k <= 0 || l > uint64(len(b)-k) {
		return "", n, errNote
	}
	tenant := port.Tenant(b[k : k+int(l)])
	b = b[k+int(l):]
	owner, k := binary.Uvarint(b)
	if k <= 0 {
		return "", n, errNote
	}
	b = b[k:]
	name, k := binary.Uvarint(b)
	if k <= 0 {
		return "", n, errNote
	}
	b = b[k:]
	l, k = binary.Uvarint(b)
	if k <= 0 || l > uint64(len(b)-k) {
		return "", n, errNote
	}
	args, err := storekit.DecodeValue(a, b[k:k+int(l)])
	if err != nil {
		return "", n, err
	}
	to, err := storekit.DecodeValue(a, b[k+int(l):])
	if err != nil {
		return "", n, err
	}
	n = reasonr.Notification{Owner: reasonr.TypeRef(owner), Name: reasonr.MethodRef(name), Args: args.Record(), To: to}
	return tenant, n, nil
}
