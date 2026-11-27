package conformance

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/tycheg/reasonr-engine/internal/bus"
	"github.com/tycheg/reasonr-engine/port"
)

// Received is a message read from the bus: Attributes are the CloudEvents
// attributes without the binding prefix (id, type, subject…), ContentType
// is the body type, Key is the order key.
type Received struct {
	Attributes  map[string]string
	ContentType string
	Key         string
	Data        []byte
}

// PublisherFactory creates the bus under test and the reading of what was
// published: read waits for the n next messages (in bus order within a key)
// and returns them. Each check gets a new bus and empty channels.
type PublisherFactory func(t *testing.T) (pub port.Publisher, read func(t *testing.T, n int) []Received)

// RunPublisher is the bus conformance set (RFC §31): every message of a
// batch is published with CloudEvents attributes, a key and the body as is
// (personal arguments — null — arrive unchanged); the order within a key
// is kept; a repeated batch publishes the same ce_ids; a notification
// without a recipient has no key and no subject.
func RunPublisher(t *testing.T, f PublisherFactory) {
	t.Run("Batch", func(t *testing.T) { testPublisherBatch(t, f) })
	t.Run("Repeat", func(t *testing.T) { testPublisherRepeat(t, f) })
}

// busMessages is a batch: the events of two entities interleaved and
// notifications with and without a recipient.
func busMessages() []port.Message {
	at := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	ev := func(pos port.Position, idx int, subject, name, data string) port.Message {
		return port.Message{Kind: port.MessageEvent, Tenant: T1, Position: pos, Index: idx, Type: "Order", Name: name,
			Subject: subject, At: at, Data: []byte(data)}
	}
	n := func(pos port.Position, idx int, subject string) port.Message {
		return port.Message{Kind: port.MessageNotification, Tenant: T1, Position: pos, Index: idx, Type: "Customer",
			Name: "rideOffered", Subject: subject, At: at, Data: []byte(`{"phone":null,"price":"350.00"}`)}
	}
	return []port.Message{
		ev(10, 0, "order-a", "placed", `{"customerName":null,"price":"350.00"}`),
		ev(11, 0, "order-b", "placed", `{"customerName":null,"price":"120.50"}`),
		ev(11, 1, "order-a", "offered", `{"driverId":"d1"}`),
		n(11, 0, "customer-1"),
		ev(12, 0, "order-b", "cancelled", `{}`),
		ev(13, 0, "order-a", "completed", `{"distanceKm":3}`),
		n(13, 0, ""),
	}
}

// check matches what was read against what was published: each message by
// ce_id, the order within a key.
func checkReceived(t *testing.T, got []Received, sent []port.Message) {
	t.Helper()
	if len(got) != len(sent) {
		t.Fatalf("%d messages read, %d published", len(got), len(sent))
	}
	byID := map[string]*port.Message{}
	for i := range sent {
		byID[bus.ID(&sent[i])] = &sent[i]
	}
	order := map[string][]string{} // key → ce_ids in order
	for _, r := range got {
		m := byID[r.Attributes["id"]]
		if m == nil {
			t.Fatalf("an extra message %v", r.Attributes)
		}
		want := map[string]string{}
		for _, a := range bus.Attributes(m) {
			want[a.Name] = a.Value
		}
		if !maps.Equal(r.Attributes, want) || r.ContentType != bus.ContentType || r.Key != m.Subject || string(r.Data) != string(m.Data) {
			t.Errorf("message %s:\n attributes %v\n want %v\n type %q, key %q, body %s", bus.ID(m), r.Attributes, want, r.ContentType, r.Key, r.Data)
		}
		if m.Subject != "" {
			order[m.Subject] = append(order[m.Subject], bus.ID(m))
		}
	}
	for key, ids := range order {
		var want []string
		for i := range sent {
			if sent[i].Subject == key {
				want = append(want, bus.ID(&sent[i]))
			}
		}
		if !slices.Equal(ids, want) {
			t.Errorf("the order of key %s: %v, want %v", key, ids, want)
		}
	}
}

func testPublisherBatch(t *testing.T, f PublisherFactory) {
	pub, read := f(t)
	pub = ready(pub)
	ctx := context.Background()
	if err := pub.Publish(ctx, nil); err != nil {
		t.Fatalf("an empty batch: %v", err)
	}
	msgs := busMessages()
	if err := pub.Publish(ctx, msgs); err != nil {
		t.Fatal(err)
	}
	checkReceived(t, read(t, len(msgs)), msgs)
}

func testPublisherRepeat(t *testing.T, f PublisherFactory) {
	pub, read := f(t)
	pub = ready(pub)
	ctx := context.Background()
	msgs := busMessages()
	if err := pub.Publish(ctx, msgs[:3]); err != nil {
		t.Fatal(err)
	}
	first := read(t, 3)
	if err := pub.Publish(ctx, msgs[:3]); err != nil { // a repeat after a fault: the same messages
		t.Fatal(err)
	}
	again := read(t, 3)
	checkReceived(t, first, msgs[:3])
	checkReceived(t, again, msgs[:3])
}

// ReceivedOf is the Received of the message m as the in-memory bus
// receiver sees it (the RunPublisher reference).
func ReceivedOf(m *port.Message) Received {
	r := Received{Attributes: map[string]string{}, ContentType: bus.ContentType, Key: m.Subject, Data: m.Data}
	for _, a := range bus.Attributes(m) {
		r.Attributes[a.Name] = a.Value
	}
	return r
}

// ready gives the adapter ready messages, as the engine does (bus.Ready).
func ready(pub port.Publisher) port.Publisher {
	r := &bus.Ready{Publisher: pub}
	if tb, ok := pub.(port.TopicBroker); ok {
		r.Template = tb.TopicTemplate()
	}
	return r
}
