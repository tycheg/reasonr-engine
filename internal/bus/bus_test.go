package bus

import (
	"slices"
	"testing"
	"time"

	"github.com/tycheg/reasonr-engine/port"
)

func TestHeaders(t *testing.T) {
	at := time.Date(2026, 1, 1, 12, 0, 0, 5, time.FixedZone("MSK", 3*3600))
	ev := port.Message{Kind: port.MessageEvent, Tenant: "acme", Position: 42, Index: 3, Type: "Order", Name: "paymentReceived",
		Subject: "0198-a", At: at, Data: []byte(`{}`)}
	got := Headers(&ev, "ce_")
	want := []Attribute{{Name: "ce_specversion", Value: "1.0"}, {Name: "ce_id", Value: "acme/42/3"}, {Name: "ce_source", Value: "reasonr/acme/Order"},
		{Name: "ce_type", Value: "Order.paymentReceived"}, {Name: "ce_subject", Value: "0198-a"}, {Name: "ce_time", Value: "2026-01-01T09:00:00.000000005Z"},
		{Name: "ce_tenant", Value: "acme"}, {Name: "ce_position", Value: "42"}, {Name: "ce_kind", Value: "event"}, {Name: "content-type", Value: "application/json"}}
	if !slices.Equal(got, want) {
		t.Errorf("event headers:\n%v\n%v", got, want)
	}
	n := port.Message{Kind: port.MessageNotification, Tenant: "acme", Position: 42, Index: 3, Type: "Customer", Name: "rideOffered", At: at}
	attrs := Attributes(&n)
	if ID(&n) != "acme/42/n3" || slices.ContainsFunc(attrs, func(a Attribute) bool { return a.Name == "subject" }) ||
		attrs[len(attrs)-1] != (Attribute{Name: "kind", Value: "notification"}) || n.Channel() != "notification.Customer" {
		t.Errorf("a notification without a recipient: %s %v", ID(&n), attrs)
	}
	if got := Topic(DefaultTopic, port.MessageEvent, "acme", "Order"); got != "reasonr.event.Order" {
		t.Errorf("channel %s", got)
	}
	if got := Topic("{tenant}.{kind}s.{type}", port.MessageNotification, "acme", "Customer"); got != "acme.notifications.Customer" {
		t.Errorf("channel by template %s", got)
	}
	for in, want := range map[string]string{`"0198-a"`: "0198-a", `42`: "42", `{"a":1}`: `{"a":1}`, `"a\"b"`: `a"b`} {
		if got := Subject([]byte(in)); got != want {
			t.Errorf("Subject(%s) = %s, want %s", in, got, want)
		}
	}
}
