package port

import (
	"context"
	"time"
)

// ForgetLog is the forget journal (RFC §20.3): who was forgotten and when.
// It is kept in a store separate from the main one: after the main
// database is restored from a backup, reasonrd pii replay repeats the key
// destruction from it, otherwise the backup would bring the forgotten back.
type ForgetLog interface {
	// Record appends entries; a repeated entry (tenant, position, subject)
	// changes nothing.
	Record(ctx context.Context, entries []Forgotten) error
	// Forgotten returns the entries at or after since in position order.
	Forgotten(ctx context.Context, since time.Time) ([]Forgotten, error)
}

// Forgotten is a forget journal entry: a subject a unit of work forgot.
type Forgotten struct {
	Tenant   Tenant
	Position Position
	At       time.Time // the time of the unit of work
	Subject  SubjectRef
}

// Publisher is the bus (RFC §31): Kafka, NATS, RabbitMQ. The message
// format is CloudEvents (engine/bus): the adapter only has to send it to
// the channel Message.Channel with the key Message.Subject.
type Publisher interface {
	// Publish sends the messages and returns once the bus has acknowledged
	// all of them; the order of messages with one key is kept. On an error
	// the batch is repeated as a whole (the receiver drops repeats by ce_id).
	// An error with ErrPermanent means a retry will not help without a
	// configuration change (no channel, no permissions).
	Publish(ctx context.Context, msgs []Message) error
}

// TopicBroker is a bus or a request transport that writes to broker topics
// by a name template (RFC §31.1). The engine derives the topics it needs
// from the models (stateless.Topics): reasonrd checks them at startup,
// reasonrd migrate creates the missing ones by the create_topics settings,
// test mode always does.
type TopicBroker interface {
	// TopicTemplate is the topic name template: {tenant}, {kind}, {type}
	// for the bus, {tenant}, {system} for the request transport.
	TopicTemplate() string
	// TopicSettings are the topic creation settings (create_topics); nil —
	// not set, the engine does not create topics.
	TopicSettings() *TopicSettings
	// DescribeTopics returns the number of partitions of those topics of
	// names that exist in the broker.
	DescribeTopics(ctx context.Context, names []string) (map[string]int, error)
	// CreateTopics creates the topics names; an existing one is not an error.
	CreateTopics(ctx context.Context, names []string, s TopicSettings) error
}

// TopicSettings are the settings of topics the engine creates.
type TopicSettings struct {
	Partitions  int               // 0 — the broker default
	Replication int               // 0 — the broker default
	Config      map[string]string // topic settings: retention.ms, cleanup.policy…
}

// The default topic name templates of the bus ({tenant}, {kind}, {type})
// and of a request transport over the bus ({tenant}, {system}).
const (
	DefaultBusTopic     = "reasonr.{kind}.{type}"
	DefaultRequestTopic = "reasonr.request.{system}"
)

// Attribute is a CloudEvents attribute (specversion, id, source, type,
// subject, time and the extensions), without the prefix of a binding (ce_
// for Kafka, ce- for HTTP).
type Attribute struct{ Name, Value string }

// MessageKind is the kind of a bus message.
type MessageKind uint8

const (
	// MessageEvent is an entity event from the event store: delivered at
	// least once, ordered within the entity.
	MessageEvent MessageKind = iota
	// MessageNotification is a notification: published after commit,
	// without delivery guarantees (RFC §13).
	MessageNotification
)

func (k MessageKind) String() string {
	if k == MessageNotification {
		return "notification"
	}
	return "event"
}

// Message is a bus message: an entity event or a notification.
type Message struct {
	Kind     MessageKind
	Tenant   Tenant
	Position Position
	// Index is the number in the unit of work: Event.Seq for an event, the
	// number among the unit's notifications for a notification.
	Index int
	Type  string // the entity or notification type (who declared it)
	Name  string // the event or notification
	// Subject is the id of the entity or of the notification recipient (a
	// string without quotes, otherwise canonical JSON): the order key; "" —
	// no recipient.
	Subject string
	At      time.Time // the time of the unit of work
	// Data are the arguments in canonical JSON (RFC §31.3), or in the bus
	// codec (ContentType); personal ones are null (RFC §20.3).
	Data []byte
	// ContentType is the media type of Data; "" — application/json.
	ContentType string
	// Topic is the channel by the bus's TopicTemplate and Attributes are the
	// CloudEvents attributes: the engine fills both before Publish.
	Topic      string
	Attributes []Attribute
}

// Headers are the headers of the binary mode of a binding: the attributes
// with the prefix prefix (ce_ for Kafka, ce- for HTTP) and content-type.
func (m *Message) Headers(prefix string) []Attribute {
	return headers(m.Attributes, prefix, m.ContentType)
}

func headers(attrs []Attribute, prefix, contentType string) []Attribute {
	out := make([]Attribute, 0, len(attrs)+1)
	for _, a := range attrs {
		out = append(out, Attribute{Name: prefix + a.Name, Value: a.Value})
	}
	if contentType == "" {
		contentType = "application/json"
	}
	return append(out, Attribute{Name: "content-type", Value: contentType})
}

// Channel is the logical channel: {kind}.{Type} (event.Order,
// notification.Customer).
func (m *Message) Channel() string { return m.Kind.String() + "." + m.Type }

// EventFeed is how the bus reads the event store (RFC §31).
type EventFeed interface {
	// ReadEvents returns the events of units of work with positions in
	// (after, upto] in position order, within a unit by entity and event;
	// at most limit (0 — all). Positions are shared by tenants, as in the
	// outbox. Version and Sealed may be left out.
	ReadEvents(ctx context.Context, after, upto Position, limit int) ([]EventRecord, error)
}

// IntegrationTransport delivers external requests to the integration
// service (HTTP, gRPC). External systems answer with ordinary commands (RFC §18).
type IntegrationTransport interface {
	// Send makes an attempt to deliver a leased request: r.Request.Key is
	// the idempotency key at the recipient, r.Deadline the deadline,
	// r.Attempts the attempt number. An error with ErrPermanent means a
	// retry will not help; others are retried (dispatch.Sender).
	Send(ctx context.Context, r *LeasedRequest) error
}

// HubLink carries messages between the server instances (PostgreSQL
// LISTEN/NOTIFY): the engine encodes the notifications to other actors, the
// forgotten subjects and the rollout changes, and hands what arrives to its
// subscribers in the process. Delivery is at most once.
type HubLink interface {
	// Send sends msg to every instance, this one included.
	Send(ctx context.Context, msg []byte) error
	// Listen starts receiving and returns once it runs: deliver gets each
	// message, one at a time, until Close.
	Listen(ctx context.Context, deliver func(msg []byte)) error
	Close() error
}
