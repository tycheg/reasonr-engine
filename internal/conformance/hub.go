package conformance

import (
	"context"
	"slices"
	"testing"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// HubFactory creates an empty notification hub.
type HubFactory func(t *testing.T) kind.NotificationHub

// RunHub runs the notification hub set: delivery to the recipient's
// subscribers (all their subscriptions and only them), tenant isolation,
// closing a subscription with Close and by cancelling the context.
func RunHub(t *testing.T, hf HubFactory) {
	t.Run("Features", func(t *testing.T) { testHubFeatures(t, hf) })
	ctx := context.Background()
	h := hf(t)
	a := reasonr.NewArena()
	actorA := reasonr.ID(1, reasonr.UUID([16]byte{15: 1}))
	actorB := a.ID(1, a.String("b")) // a string identifier — in the arena
	actorC := reasonr.ID(1, reasonr.UUID([16]byte{15: 3}))
	subscribe := func(c context.Context, tenant port.Tenant, actor reasonr.Value) kind.Subscription {
		t.Helper()
		s, err := h.Subscribe(c, tenant, a, actor)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	a1, a2 := subscribe(ctx, T1, actorA), subscribe(ctx, T1, actorA)
	b1 := subscribe(ctx, T1, actorB)
	other := subscribe(ctx, T2, actorA)
	note := func(to reasonr.Value, text string) reasonr.Notification {
		return reasonr.Notification{Owner: 1, Name: 2, Args: a.Record(0, a.String(text)), To: to}
	}
	ns := []reasonr.Notification{note(actorA, "a"), note(a.ID(1, a.String("b")), "b"), note(actorC, "c"), note(reasonr.None, "nobody")}
	if err := h.Publish(ctx, T1, a, ns); err != nil {
		t.Fatal(err)
	}
	expect := func(s kind.Subscription, text string, to reasonr.Value) {
		t.Helper()
		select {
		case d, ok := <-s.C():
			if !ok {
				t.Fatal("the subscription is closed")
			}
			args := d.Arena.Fields(d.Notification.Args)
			if d.Notification.Owner != 1 || d.Notification.Name != 2 || len(args) != 1 || d.Arena.Text(args[0]) != text ||
				!reasonr.Equal(d.Arena, d.Notification.To, a, to) {
				t.Fatalf("delivered %s, want %q", d.Arena.Format(d.Notification.Args.Value()), text)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("notification %q is not delivered", text)
		}
	}
	quiet := func(s kind.Subscription) {
		t.Helper()
		select {
		case d := <-s.C():
			t.Fatalf("an extra notification %s", d.Arena.Format(d.Notification.Args.Value()))
		case <-time.After(50 * time.Millisecond):
		}
	}
	expect(a1, "a", actorA)
	expect(a2, "a", actorA)
	expect(b1, "b", actorB)
	quiet(a1)
	quiet(b1)
	quiet(other)

	// A closed subscription gets no more notifications, the channel is closed.
	if err := a2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.Publish(ctx, T1, a, ns[:1]); err != nil {
		t.Fatal(err)
	}
	expect(a1, "a", actorA)
	if _, ok := <-a2.C(); ok {
		t.Fatal("a closed subscription got a notification")
	}
	if err := a2.Close(); err != nil {
		t.Fatalf("a repeated Close: %v", err)
	}

	// Cancelling the context closes the subscription.
	cctx, cancel := context.WithCancel(ctx)
	c := subscribe(cctx, T1, actorC)
	cancel()
	select {
	case _, ok := <-c.C():
		if ok {
			t.Fatal("a notification came after cancel")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the context did not close the subscription")
	}
	for _, s := range []kind.Subscription{a1, b1, other} {
		_ = s.Close()
	}
	runForgetHub(t, h)
}

// runForgetHub checks the broadcast of forgotten subjects, if the hub does
// it: a subscriber gets the tenant and the subjects.
func runForgetHub(t *testing.T, h kind.NotificationHub) {
	fh, ok := h.(ports.ForgetHub)
	if !ok {
		return
	}
	got := make(chan []port.SubjectRef, 4)
	fh.OnForgotten(func(tenant port.Tenant, subjects []port.SubjectRef) {
		if tenant == T2 {
			got <- subjects
		}
	})
	want := []port.SubjectRef{{Type: "Person", ID: `"a"`}, {Type: "Person", ID: `"b"`}}
	if err := fh.PublishForgotten(context.Background(), T2, want); err != nil {
		t.Fatal(err)
	}
	select {
	case s := <-got:
		if !slices.Equal(s, want) {
			t.Fatalf("forgotten: %v, want %v", s, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the forgotten subjects are not delivered")
	}
}

// testHubFeatures: the message "the feature flag rollout changed" reaches
// the subscribers, its own included (RFC §23.4). Skipped if the hub does
// not broadcast it.
func testHubFeatures(t *testing.T, hf HubFactory) {
	fh, ok := hf(t).(ports.FeatureHub)
	if !ok {
		t.Skip("the hub does not broadcast feature flag changes")
	}
	got := make(chan struct{}, 4)
	fh.OnFeatures(func() { got <- struct{}{} })
	if err := fh.PublishFeatures(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the message did not arrive")
	}
}
