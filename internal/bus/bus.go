// Package bus is the bus message format (RFC §31): CloudEvents 1.0 in
// binary mode. A bus adapter gets a port.Message and sends the Headers
// attributes as headers and Message.Data as the body; CloudEvents has
// bindings for Kafka, NATS, AMQP, MQTT, so the format is one for any bus.
package bus

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/codecs"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// SpecVersion is the CloudEvents version.
const SpecVersion = "1.0"

// ContentType is the message body type.
const ContentType = "application/json"

// DefaultTopic is the default bus channel name template.
const DefaultTopic = port.DefaultBusTopic

// DefaultRequestTopic is the default channel name template of external
// requests (a request transport over the bus, RFC §18).
const DefaultRequestTopic = port.DefaultRequestTopic

// Attribute is a CloudEvents attribute.
type Attribute = port.Attribute

// Ready is a publisher that fills the topic and the CloudEvents attributes
// of each message before the bus adapter gets it (port.Message).
type Ready struct {
	port.Publisher
	// Template is the topic name template; "" — DefaultTopic.
	Template string
}

// Publish fills the messages and publishes them.
func (r *Ready) Publish(ctx context.Context, msgs []port.Message) error {
	out := make([]port.Message, len(msgs))
	for i, m := range msgs {
		Prepare(&m, r.Template)
		out[i] = m
	}
	return r.Publisher.Publish(ctx, out)
}

// Prepare fills the topic of message m by the template (""
// — DefaultTopic) and its CloudEvents attributes.
func Prepare(m *port.Message, template string) {
	if template == "" {
		template = DefaultTopic
	}
	m.Topic = Topic(template, m.Kind, m.Tenant, m.Type)
	m.Attributes = Attributes(m)
}

// FromEvent is the bus message of an event of the event store.
func FromEvent(e *port.EventRecord) port.Message {
	return port.Message{Kind: port.MessageEvent, Tenant: e.Tenant, Position: e.Position, Index: e.Seq, Type: e.Type, Name: e.Name,
		Subject: Subject(e.ID), At: e.At.UTC(), Data: e.Args}
}

// RequestAttributes are the CloudEvents attributes of an external request
// over a bus (RFC §18): id is the idempotency key, type
// {System}.{request}, source reasonr/{tenant}/{System}, time the time of
// the unit of work, the extensions tenant, attempt and deadline (the timer
// deadline, if any).
func RequestAttributes(r *kind.LeasedRequest) []Attribute {
	attrs := []Attribute{
		{Name: "specversion", Value: SpecVersion},
		{Name: "id", Value: r.Key},
		{Name: "source", Value: "reasonr/" + string(r.Tenant) + "/" + r.System},
		{Name: "type", Value: r.System + "." + r.Name},
		{Name: "time", Value: r.At.UTC().Format(time.RFC3339Nano)},
		{Name: "tenant", Value: string(r.Tenant)},
		{Name: "attempt", Value: strconv.Itoa(max(r.Attempts, 1))},
	}
	if !r.Deadline.IsZero() {
		attrs = append(attrs, Attribute{Name: "deadline", Value: r.Deadline.UTC().Format(time.RFC3339Nano)})
	}
	return attrs
}

// ID is ce_id: {tenant}/{position}/{number}; a notification number has the
// prefix n — the event and notification numbers of a unit are independent.
func ID(m *port.Message) string {
	n := strconv.Itoa(m.Index)
	if m.Kind == port.MessageNotification {
		n = "n" + n
	}
	return string(m.Tenant) + "/" + strconv.FormatUint(uint64(m.Position), 10) + "/" + n
}

// Attributes returns the CloudEvents attributes of a message: the required
// ones (specversion, id, source, type), subject (if there is a recipient),
// time and the extensions tenant, position, kind. Bindings pass
// datacontenttype in their own header (content-type), so it is not here.
func Attributes(m *port.Message) []Attribute {
	attrs := []Attribute{
		{Name: "specversion", Value: SpecVersion},
		{Name: "id", Value: ID(m)},
		{Name: "source", Value: "reasonr/" + string(m.Tenant) + "/" + m.Type},
		{Name: "type", Value: m.Type + "." + m.Name},
	}
	if m.Subject != "" {
		attrs = append(attrs, Attribute{Name: "subject", Value: m.Subject})
	}
	return append(attrs,
		Attribute{Name: "time", Value: m.At.UTC().Format(time.RFC3339Nano)},
		Attribute{Name: "tenant", Value: string(m.Tenant)},
		Attribute{Name: "position", Value: strconv.FormatUint(uint64(m.Position), 10)},
		Attribute{Name: "kind", Value: m.Kind.String()})
}

// Headers are the binary mode headers: the attributes with the prefix
// prefix (ce_ for Kafka, ce- for HTTP) and content-type.
func Headers(m *port.Message, prefix string) []Attribute {
	if m.Attributes == nil {
		c := *m
		c.Attributes = Attributes(m)
		m = &c
	}
	return m.Headers(prefix)
}

// Recoder is a publisher that re-encodes message bodies from the canonical
// JSON the engine keeps into the bus codec (bus.codec) before publishing.
type Recoder struct {
	port.Publisher
	Catalog kind.Catalog
	Codec   port.Codec
}

// Publish re-encodes the messages and publishes them.
func (r *Recoder) Publish(ctx context.Context, msgs []port.Message) error {
	out := make([]port.Message, len(msgs))
	a := reasonr.GetArena()
	defer reasonr.PutArena(a)
	for i, m := range msgs {
		s, err := r.Catalog.Schema(m.Tenant)
		if err != nil {
			return err
		}
		if m.Data, err = Recode(s, a, &m, r.Codec); err != nil {
			return fmt.Errorf("bus: %s.%s: %w", m.Type, m.Name, err)
		}
		m.ContentType = r.Codec.ContentType()
		out[i] = m
	}
	return r.Publisher.Publish(ctx, out)
}

// Recode returns the arguments of message m (canonical JSON) in codec c.
func Recode(s *reasonr.Schema, a *reasonr.Arena, m *port.Message, c port.Codec) ([]byte, error) {
	ti := s.Lookup(m.Type)
	if ti == nil {
		return nil, fmt.Errorf("no type %s", m.Type)
	}
	var mi *reasonr.MethodInfo
	for i := range ti.Methods {
		if ti.Methods[i].Name == m.Name {
			mi = &ti.Methods[i]
		}
	}
	if mi == nil {
		return nil, fmt.Errorf("no %s in %s", m.Name, m.Type)
	}
	o := codecs.Options{SealedPersonal: true}
	args, err := codecs.JSONOf(s, o).DecodeArgs(a, mi.Params, m.Data)
	if err != nil {
		return nil, err
	}
	return codecs.New(c, s, o).AppendFields(nil, a, mi.Params, args)
}

// Topic expands the channel name template: {tenant}, {kind}, {type}.
func Topic(template string, kind port.MessageKind, tenant port.Tenant, typ string) string {
	return strings.NewReplacer("{tenant}", string(tenant), "{kind}", kind.String(), "{type}", typ).Replace(template)
}

// RequestTopic expands the channel name template of external requests:
// {tenant}, {system}.
func RequestTopic(template string, tenant port.Tenant, system string) string {
	return strings.NewReplacer("{tenant}", string(tenant), "{system}", system).Replace(template)
}

// Subject returns Message.Subject by the canonical JSON of an identifier: a
// string without quotes, otherwise the JSON as is.
func Subject(id []byte) string {
	var s string
	if len(id) > 0 && id[0] == '"' && json.Unmarshal(id, &s) == nil {
		return s
	}
	return string(id)
}

// Encoder encodes bus messages by the tenant's schema.
type Encoder struct {
	schema *reasonr.Schema
	json   *codecs.Values
}

// NewEncoder returns an encoder of the schema s.
func NewEncoder(s *reasonr.Schema) *Encoder {
	c := codecs.JSONOf(s, codecs.Options{SealedPersonal: true}) // personal ones are null
	return &Encoder{schema: s, json: c}
}

// Event encodes an event of the entity id of type typ of a unit of work.
func (e *Encoder) Event(tenant port.Tenant, pos port.Position, at time.Time, a *reasonr.Arena, typ reasonr.TypeRef, id reasonr.Value, ev *reasonr.Event) (port.EventRecord, error) {
	ti := e.schema.Type(typ)
	mi := &ti.Methods[ev.Name]
	idJSON, err := e.json.Append(nil, a, reasonr.Type{Kind: reasonr.KindID, Ref: typ}, id)
	if err != nil {
		return port.EventRecord{}, fmt.Errorf("bus: id %s: %w", ti.Name, err)
	}
	data, err := e.args(a, mi, ev.Args)
	if err != nil {
		return port.EventRecord{}, fmt.Errorf("bus: %s.%s: %w", ti.Name, mi.Name, err)
	}
	return port.EventRecord{Tenant: tenant, Position: pos, Seq: int(ev.Seq), Type: ti.Name, ID: idJSON, Name: mi.Name, Args: data, At: at}, nil
}

// Notifications encodes the notifications of a unit of work; the number is
// the order in ns. A recipient without a value (a notification to the
// initiator of a call by a role) gives a message without a subject.
func (e *Encoder) Notifications(tenant port.Tenant, pos port.Position, at time.Time, a *reasonr.Arena, ns []reasonr.Notification) ([]port.Message, error) {
	out := make([]port.Message, 0, len(ns))
	for i := range ns {
		n := &ns[i]
		ti := e.schema.Type(n.Owner)
		mi := &ti.Methods[n.Name]
		m := port.Message{Kind: port.MessageNotification, Tenant: tenant, Position: pos, Index: i, Type: ti.Name, Name: mi.Name, At: at}
		if n.To.Kind != reasonr.KindNone {
			to, err := e.json.Append(nil, a, reasonr.Type{Kind: reasonr.KindID, Ref: n.To.Type}, n.To)
			if err != nil {
				return nil, fmt.Errorf("bus: recipient of %s.%s: %w", ti.Name, mi.Name, err)
			}
			m.Subject = Subject(to)
		}
		data, err := e.args(a, mi, n.Args)
		if err != nil {
			return nil, fmt.Errorf("bus: %s.%s: %w", ti.Name, mi.Name, err)
		}
		m.Data = data
		out = append(out, m)
	}
	return out, nil
}

// args encodes the arguments; personal ones are null.
func (e *Encoder) args(a *reasonr.Arena, mi *reasonr.MethodInfo, args reasonr.Record) ([]byte, error) {
	return e.json.AppendFields(nil, a, mi.Params, WithoutPersonal(a, mi.Params, args))
}

// WithoutPersonal returns the arguments with personal ones as none (a copy
// in the arena a; without personal parameters — the same arguments): the
// bus storage is out of forget's reach (RFC §20.3).
func WithoutPersonal(a *reasonr.Arena, params []reasonr.FieldInfo, args reasonr.Record) reasonr.Record {
	var strip []int
	for i := range params {
		if params[i].Personal != nil {
			strip = append(strip, i)
		}
	}
	if strip == nil {
		return args
	}
	m := a.Mark()
	for i, v := range a.Elems(args.Value()) {
		if slices.Contains(strip, i) {
			v = reasonr.None
		}
		a.Push(v)
	}
	return a.End(reasonr.KindRecord, args.Value().Type, m).Record()
}
