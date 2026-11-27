package porttest

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tycheg/reasonr-engine/port"
)

// Received is a message as a test read it from the broker: the
// CloudEvents attributes without the prefix of the binding, the content
// type, the key and the body.
type Received struct {
	Attributes  map[string]string
	ContentType string
	Key         string
	Data        []byte
}

// PublisherFactory opens the bus under test with the topics of
// BusTopics created, and the reading of what was published: read waits for
// the n next messages (in bus order within a key). Each call gets a new
// bus and empty topics.
type PublisherFactory func(t *testing.T) (pub port.Publisher, read func(t *testing.T, n int) []Received)

// TransportFactory opens the integration transport under test with the
// topic of RequestTopic created (a transport over a bus), and the reading
// of what it sent.
type TransportFactory func(t *testing.T) (tr port.IntegrationTransport, read func(t *testing.T, n int) []Received)

// topic expands a topic name template.
func topic(template string, vars ...string) string {
	return strings.NewReplacer(vars...).Replace(template)
}

// BusTopics are the topics RunPublisher publishes to by the template.
func BusTopics(template string) []string {
	return []string{topic(template, "{tenant}", string(T1), "{kind}", "event", "{type}", "Shop.Order"),
		topic(template, "{tenant}", string(T1), "{kind}", "notification", "{type}", "Shop.Customer")}
}

// RequestTopic is the topic RunTransport sends to by the template.
func RequestTopic(template string) string {
	return topic(template, "{tenant}", string(T1), "{system}", "Bank")
}

// busMessages are ready messages: the events of two entities interleaved
// and notifications with and without a recipient.
func busMessages(template string) []port.Message {
	topics := BusTopics(template)
	msg := func(kind port.MessageKind, pos port.Position, idx int, subject, name, data string) port.Message {
		m := port.Message{Kind: kind, Tenant: T1, Position: pos, Index: idx, Type: "Shop.Order", Name: name, Subject: subject, At: Start,
			Data: []byte(data), Topic: topics[0]}
		if kind == port.MessageNotification {
			m.Type, m.Topic = "Shop.Customer", topics[1]
		}
		id := fmt.Sprintf("%s/%d/%s%d", T1, pos, kind, idx)
		m.Attributes = []port.Attribute{{Name: "specversion", Value: "1.0"}, {Name: "id", Value: id},
			{Name: "source", Value: "reasonr/t1/" + m.Type}, {Name: "type", Value: m.Type + "." + name}}
		if subject != "" {
			m.Attributes = append(m.Attributes, port.Attribute{Name: "subject", Value: subject})
		}
		m.Attributes = append(m.Attributes, port.Attribute{Name: "time", Value: Start.Format(time.RFC3339Nano)})
		return m
	}
	return []port.Message{
		msg(port.MessageEvent, 1, 0, "order-a", "placed", `{"customer":null,"price":"350.00"}`),
		msg(port.MessageEvent, 2, 0, "order-b", "placed", `{"customer":null,"price":"120.50"}`),
		msg(port.MessageEvent, 2, 1, "order-a", "offered", `{"driver":"d1"}`),
		msg(port.MessageNotification, 2, 0, "customer-1", "offered", `{"phone":null}`),
		msg(port.MessageEvent, 3, 0, "order-b", "cancelled", `{}`),
		msg(port.MessageEvent, 4, 0, "order-a", "completed", `{"km":3}`),
		msg(port.MessageNotification, 4, 0, "", "news", `{}`),
	}
}

// checkReceived matches what was read against what was sent: each message
// by its id attribute, the order within a key.
func checkReceived(t *testing.T, got []Received, sent []port.Message) {
	t.Helper()
	if len(got) != len(sent) {
		t.Fatalf("%d messages read, %d published", len(got), len(sent))
	}
	byID := map[string]*port.Message{}
	for i := range sent {
		byID[attr(sent[i].Attributes, "id")] = &sent[i]
	}
	order := map[string][]string{}
	for _, r := range got {
		m := byID[r.Attributes["id"]]
		if m == nil {
			t.Fatalf("an extra message %v", r.Attributes)
		}
		want := map[string]string{}
		for _, a := range m.Attributes {
			want[a.Name] = a.Value
		}
		if !maps.Equal(r.Attributes, want) || r.ContentType != "application/json" || r.Key != m.Subject || !bytes.Equal(r.Data, m.Data) {
			t.Errorf("message %s:\n attributes %v\n want %v\n type %q, key %q, body %s", want["id"], r.Attributes, want, r.ContentType, r.Key, r.Data)
		}
		if m.Subject != "" {
			order[m.Subject] = append(order[m.Subject], want["id"])
		}
	}
	for key, got := range order {
		var want []string
		for i := range sent {
			if sent[i].Subject == key {
				want = append(want, attr(sent[i].Attributes, "id"))
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("the order of key %s: %v, want %v", key, got, want)
		}
	}
}

func attr(attrs []port.Attribute, name string) string {
	for _, a := range attrs {
		if a.Name == name {
			return a.Value
		}
	}
	return ""
}

// template is the topic template of a bus or transport, or def.
func template(x any, def string) string {
	if tb, ok := x.(port.TopicBroker); ok && tb.TopicTemplate() != "" {
		return tb.TopicTemplate()
	}
	return def
}

// RunPublisher checks a bus (port.Publisher): a batch of ready messages is
// published to their topics with their attributes, key and body as is; the
// order within a key is kept; a repeated batch publishes the same messages.
func RunPublisher(t *testing.T, f PublisherFactory) {
	t.Run("Batch", func(t *testing.T) {
		pub, read := f(t)
		must(t, pub.Publish(ctx(t), nil))
		msgs := busMessages(template(pub, port.DefaultBusTopic))
		must(t, pub.Publish(ctx(t), msgs))
		checkReceived(t, read(t, len(msgs)), msgs)
	})
	t.Run("Repeat", func(t *testing.T) {
		pub, read := f(t)
		msgs := busMessages(template(pub, port.DefaultBusTopic))[:3]
		must(t, pub.Publish(ctx(t), msgs))
		first := read(t, 3)
		must(t, pub.Publish(ctx(t), msgs)) // a repeat after a fault: the same messages
		checkReceived(t, first, msgs)
		checkReceived(t, read(t, 3), msgs)
	})
}

// RunTransport checks an integration transport over a bus: a ready
// request goes to its topic with the idempotency key as the key, the
// attributes and the body.
func RunTransport(t *testing.T, f TransportFactory) {
	tr, read := f(t)
	r := &port.LeasedRequest{Tenant: T1, Position: 7, At: Start, Attempts: 2, Deadline: Start.Add(time.Minute),
		System: "Bank", Name: "charge", Key: "0a0b0000-0000-0000-0000-000000000000", Body: []byte(`{"amount":"5.00"}`),
		Attributes: []port.Attribute{{Name: "specversion", Value: "1.0"}, {Name: "id", Value: "0a0b0000-0000-0000-0000-000000000000"},
			{Name: "source", Value: "reasonr/t1/Bank"}, {Name: "type", Value: "Bank.charge"}, {Name: "attempt", Value: "2"}},
		Topic: RequestTopic(template(tr, port.DefaultRequestTopic))}
	must(t, tr.Send(ctx(t), r))
	got := read(t, 1)[0]
	want := map[string]string{}
	for _, a := range r.Attributes {
		want[a.Name] = a.Value
	}
	if !maps.Equal(got.Attributes, want) || got.Key != r.Key || got.ContentType != "application/json" || !bytes.Equal(got.Data, r.Body) {
		t.Fatalf("a sent request: %+v", got)
	}
}

// RunHubLink checks a link between instances (port.HubLink): what one link
// sends every link on the same infrastructure gets, the sender included;
// large messages too. open gives a new link to the same infrastructure on
// each call.
func RunHubLink(t *testing.T, open func(t *testing.T) port.HubLink) {
	a, b := open(t), open(t)
	type inbox struct {
		mu  sync.Mutex
		got [][]byte
	}
	listen := func(l port.HubLink) *inbox {
		in := &inbox{}
		must(t, l.Listen(ctx(t), func(msg []byte) {
			in.mu.Lock()
			in.got = append(in.got, bytes.Clone(msg))
			in.mu.Unlock()
		}))
		return in
	}
	ia, ib := listen(a), listen(b)
	big := bytes.Repeat([]byte("é"), 20000)
	msgs := [][]byte{[]byte("hello"), big, []byte("bye")}
	for _, m := range msgs {
		must(t, a.Send(ctx(t), m))
	}
	for name, in := range map[string]*inbox{"the sender": ia, "another instance": ib} {
		deadline := time.Now().Add(10 * time.Second)
		for {
			in.mu.Lock()
			n := len(in.got)
			got := slices.Clone(in.got)
			in.mu.Unlock()
			if n >= len(msgs) {
				for _, m := range msgs {
					if !slices.ContainsFunc(got, func(g []byte) bool { return bytes.Equal(g, m) }) {
						t.Fatalf("%s did not get a message of %d bytes", name, len(m))
					}
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s got %d messages of %d", name, n, len(msgs))
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	must(t, a.Close())
	must(t, b.Close())
}

// RunKMS checks a KMS: a key wrapped in a context unwraps in it, also with
// another instance of the KMS (another process), and not in another
// context. open gives a new instance with the same master key.
func RunKMS(t *testing.T, open func(t *testing.T) port.KMS) {
	k1, k2 := open(t), open(t)
	plain := []byte("0123456789abcdef0123456789abcdef")
	wrapped, err := k1.Wrap(context.Background(), "tenant/t1", plain)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if bytes.Contains(wrapped, plain) {
		t.Fatal("the wrapped key holds the plain key")
	}
	for _, k := range []port.KMS{k1, k2} {
		got, err := k.Unwrap(context.Background(), "tenant/t1", wrapped)
		if err != nil || !bytes.Equal(got, plain) {
			t.Fatalf("Unwrap: %x %v", got, err)
		}
	}
	if got, err := k2.Unwrap(context.Background(), "tenant/t2", wrapped); err == nil && bytes.Equal(got, plain) {
		t.Fatal("a key unwraps in another context")
	}
}
