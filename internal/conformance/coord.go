package conformance

import (
	"context"
	"testing"
	"time"

	"github.com/tycheg/reasonr-engine/port"
)

// RunCoordinator runs the set of the effect stream coordination port:
// cursors, leases, dead letters.
func RunCoordinator(t *testing.T, newCoord func(t *testing.T) port.Coordinator) {
	ctx := context.Background()
	c := newCoord(t)
	if pos, err := c.Cursor(ctx, "events"); err != nil || pos != 0 {
		t.Fatalf("the cursor of a new consumer: %d %v", pos, err)
	}
	for _, p := range []port.Position{5, 9} {
		if err := c.SetCursor(ctx, "events", p); err != nil {
			t.Fatal(err)
		}
	}
	if pos, _ := c.Cursor(ctx, "events"); pos != 9 {
		t.Fatalf("cursor %d", pos)
	}
	if pos, _ := c.Cursor(ctx, "requests"); pos != 0 {
		t.Fatal("the consumers' cursors are not separate")
	}
	steps := []struct {
		holder string
		at     time.Duration
		want   bool
	}{{"a", 0, true}, {"b", time.Second, false}, {"a", 2 * time.Second, true}, {"b", 31 * time.Second, false}, {"b", 33 * time.Second, true}, {"a", 34 * time.Second, false}}
	for i, s := range steps {
		ok, err := c.Acquire(ctx, "dispatch:events", s.holder, Start.Add(s.at), 30*time.Second)
		if err != nil || ok != s.want {
			t.Fatalf("lease %d (%s at %s): %v %v", i, s.holder, s.at, ok, err)
		}
	}
	d := port.DeadLetter{Consumer: "requests", Position: 7, Item: 1, Tenant: T1, Reason: "HTTP 422", At: Start}
	for range 2 {
		if err := c.Bury(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Bury(ctx, port.DeadLetter{Consumer: "events", Position: 3, Item: -1, Tenant: T2, Reason: "x", At: Start}); err != nil {
		t.Fatal(err)
	}
	list, err := c.DeadLetters(ctx, "requests")
	if err != nil || len(list) != 1 || list[0].Item != 1 || list[0].Tenant != T1 || list[0].Reason != "HTTP 422" || !list[0].At.Equal(Start) {
		t.Fatalf("dead letters: %+v %v", list, err)
	}
	if all, _ := c.DeadLetters(ctx, ""); len(all) != 2 || all[0].Position != 3 {
		t.Fatalf("all dead letters by position: %+v", all)
	}
	if err := c.Resurrect(ctx, d); err != nil {
		t.Fatal(err)
	}
	if list, _ := c.DeadLetters(ctx, "requests"); len(list) != 0 {
		t.Fatal("the letter is not deleted")
	}
}
