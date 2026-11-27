package conformance

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/internal/rows"

	"github.com/tycheg/reasonr-engine/internal/codecs"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/bus"
	"github.com/tycheg/reasonr-engine/internal/dispatch"
	"github.com/tycheg/reasonr-engine/port"
)

// testEventFeed: the event store gives the bus the events of every tenant
// with positions in (after, upto] in position order (RFC §31): the kind,
// the tenant, the position, the number (Event.Seq), the type, the name,
// the entity id as a string, the unit time, the arguments in canonical
// JSON with personal ones null; limit limits the number of events; a
// repeated write of a unit does not double them. Skipped if the store
// cannot be an event store.
func testEventFeed(t *testing.T, sf Factory) {
	f := newFixture(t, sf, options{})
	sink, ok := f.st.Unit.(ports.EventStore)
	feed, ok2 := rows.As[port.EventFeed](f.st.Unit)
	if !ok || !ok2 {
		t.Skip("the store cannot be an event store for the bus")
	}
	ctx := context.Background()
	b1, b2, k := f.id("Box"), f.id("Box"), f.id("Keeper")
	f.exec(admin, b1, "Put", f.kind("small"), reasonr.Int(1), reasonr.None)
	f.tenant = T2
	f.exec(admin, b2, "Put", f.kind("big"), reasonr.Int(2), k)
	f.tenant = T1
	f.exec(actor(k), k, "Join", f.a.String("Anne"), f.a.String("A"))
	f.exec(actor(k), k, "Poke", k) // notifications only: no events
	f.exec(admin, b1, "Put", f.kind("big"), reasonr.Int(3), reasonr.None)
	units := f.units(0)
	if len(units) != 5 {
		t.Fatalf("%d units of work", len(units))
	}
	c := &dispatch.Events{Sink: sink}
	for range 2 { // a repeated write
		for i := range units {
			if err := c.Deliver(ctx, &units[i], -1, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	last := units[4].Position
	subject := func(id reasonr.Value) string {
		js, err := codecs.JSONOf(f.s, codecs.Options{}).Append(nil, f.a, reasonr.Type{Kind: reasonr.KindID, Ref: id.Type}, id)
		if err != nil {
			t.Fatal(err)
		}
		return bus.Subject(js)
	}
	type want struct {
		tenant  port.Tenant
		unit    int
		typ     string
		name    string
		subject string
		data    string
	}
	all := []want{
		{T1, 0, "Box", "stored", subject(b1), `{"boxKind":"small","boxSize":1,"keeper":null}`},
		{T2, 1, "Box", "stored", subject(b2), `{"boxKind":"big","boxSize":2,"keeper":"` + subject(k) + `"}`},
		{T1, 2, "Keeper", "joined", subject(k), `{"keeperName":null,"keeperCode":"A"}`},
		{T1, 4, "Box", "stored", subject(b1), `{"boxKind":"big","boxSize":3,"keeper":null}`},
	}
	check := func(evs []port.EventRecord, exp []want) {
		t.Helper()
		got := make([]port.Message, len(evs))
		for i := range evs {
			got[i] = bus.FromEvent(&evs[i])
		}
		if len(got) != len(exp) {
			t.Fatalf("%d events, want %d: %+v", len(got), len(exp), got)
		}
		for i, w := range exp {
			m := got[i]
			u := units[w.unit]
			if m.Kind != port.MessageEvent || m.Tenant != w.tenant || m.Position != u.Position || m.Index != int(u.Entities[0].Events[0].Seq) ||
				m.Type != w.typ || m.Name != w.name || m.Subject != w.subject || !m.At.Equal(Start) {
				t.Errorf("event %d: %+v, want %+v at position %d", i, m, w, u.Position)
			}
			var g, e any
			if err := json.Unmarshal(m.Data, &g); err != nil {
				t.Fatalf("event %d: arguments %s: %v", i, m.Data, err)
			}
			_ = json.Unmarshal([]byte(w.data), &e)
			if !reflect.DeepEqual(g, e) {
				t.Errorf("event %d: arguments %s, want %s", i, m.Data, w.data)
			}
		}
	}
	got, err := feed.ReadEvents(ctx, 0, last, 0)
	if err != nil {
		t.Fatal(err)
	}
	check(got, all)
	if got, err = feed.ReadEvents(ctx, units[0].Position, units[3].Position, 0); err != nil {
		t.Fatal(err)
	}
	check(got, all[1:3])
	if got, err = feed.ReadEvents(ctx, 0, last, 2); err != nil {
		t.Fatal(err)
	}
	check(got, all[:2])
	if got, err = feed.ReadEvents(ctx, last, last+100, 0); err != nil || len(got) != 0 {
		t.Fatalf("after the last position: %d, %v", len(got), err)
	}
}
