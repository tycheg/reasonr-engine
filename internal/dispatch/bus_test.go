package dispatch_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/memory"
	"github.com/tycheg/reasonr-engine/internal/bus"
	"github.com/tycheg/reasonr-engine/internal/codecs"
	"github.com/tycheg/reasonr-engine/internal/conformance"
	"github.com/tycheg/reasonr-engine/internal/dispatch"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// ids returns the ce_ids of the messages in order.
func ids(msgs []port.Message) []string {
	out := make([]string, len(msgs))
	for i := range msgs {
		out[i] = bus.ID(&msgs[i])
	}
	return out
}

// TestBus: the bus publishes every event from the event store in position
// order, but not beyond the cursor of the events consumer; units without
// events only move the cursor.
func TestBus(t *testing.T) {
	f := newFixture(t)
	pub := &memory.Bus{}
	b := &dispatch.Bus{Feed: f.store, Publisher: pub, Coord: f.coord, LeaseTTL: time.Minute, Poll: time.Hour} // only Events.OnAppend wakes it
	b1, b2 := f.box(), f.box()
	p1 := f.put(b1, 1)
	p2 := f.exec(b1, "Arm", reasonr.Duration(time.Minute))
	p3 := f.put(b2, 2)
	if n, err := b.PublishDue(context.Background()); n != 0 || err != nil {
		t.Fatalf("published %d before the event store wrote, %v", n, err)
	}
	events := &dispatch.Events{Sink: f.store, OnAppend: b.Wake}
	stop := run(f.dispatcher(events))
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = b.Run(ctx)
		close(done)
	}()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool { return len(pub.Messages()) == 3 })
	p4 := f.put(b2, 3)
	waitFor(t, func() bool { return len(pub.Messages()) == 4 })
	msgs := pub.Messages()
	want := []string{fmt.Sprintf("default/%d/0", p1), fmt.Sprintf("default/%d/0", p2), fmt.Sprintf("default/%d/0", p3), fmt.Sprintf("default/%d/0", p4)}
	if got := ids(msgs); !slices.Equal(got, want) {
		t.Fatalf("ce_id %v, want %v", got, want)
	}
	if m := msgs[0]; m.Channel() != "event.Box" || m.Name != "stored" || string(m.Data) != `{"boxKind":"small","boxSize":1,"keeper":null}` {
		t.Errorf("the first event: %+v %s", m, m.Data)
	}
	if msgs[1].Name != "armed" || msgs[0].Subject != msgs[1].Subject || msgs[0].Subject == msgs[2].Subject {
		t.Errorf("event entities: %+v", msgs)
	}
	if c, _ := f.coord.Cursor(context.Background(), dispatch.BusCursor); c != p4 {
		t.Errorf("bus cursor %d, want %d", c, p4)
	}

	// A stopped bus gives up the lease: another instance continues without
	// waiting for LeaseTTL.
	cancel()
	<-done
	next := &dispatch.Bus{Feed: f.store, Publisher: pub, Coord: f.coord, LeaseTTL: time.Minute, Poll: 10 * time.Millisecond}
	ctx2, cancel2 := context.WithCancel(context.Background())
	done2 := make(chan struct{})
	go func() {
		_ = next.Run(ctx2)
		close(done2)
	}()
	defer func() { cancel2(); <-done2 }()
	f.put(b2, 4)
	waitFor(t, func() bool { return len(pub.Messages()) == 5 })
}

func (fd feed) messages() []port.Message {
	out := make([]port.Message, len(fd))
	for i := range fd {
		out[i] = bus.FromEvent(&fd[i])
	}
	return out
}

// feed is an event store of ready records.
type feed []port.EventRecord

func (fd feed) ReadEvents(_ context.Context, after, upto port.Position, limit int) ([]port.EventRecord, error) {
	var out []port.EventRecord
	for _, m := range fd {
		if m.Position > after && m.Position <= upto && (limit == 0 || len(out) < limit) {
			out = append(out, m)
		}
	}
	return out, nil
}

// units builds the events of units: n[i] events at position i+1.
func units(n ...int) feed {
	var fd feed
	for i, k := range n {
		for j := range k {
			fd = append(fd, port.EventRecord{Tenant: tenant, Position: port.Position(i + 1), Seq: j, Type: "Box", Name: "stored", ID: []byte(`"b"`)})
		}
	}
	return fd
}

// TestBusBatch: a batch does not cut a unit of work — the last incomplete
// unit is postponed to the next batch, a unit larger than a batch is
// published whole; the cursor moves only by published units; events after
// the cursor of the events consumer are not published.
func TestBusBatch(t *testing.T) {
	fd := units(3, 1, 2, 0, 1, 1)
	coord := memory.NewCoordinator()
	_ = coord.SetCursor(context.Background(), dispatch.EventsConsumer, 5)
	pub := &batches{}
	b := &dispatch.Bus{Feed: fd, Publisher: pub, Coord: coord, Batch: 2}
	n, err := b.PublishDue(context.Background())
	if err != nil || n != len(fd)-1 {
		t.Fatalf("published %d of %d, %v", n, len(fd)-1, err)
	}
	if want := []string{"1/0 1/1 1/2", "2/0", "3/0 3/1", "5/0"}; !slices.Equal(pub.seen, want) {
		t.Errorf("batches %q, want %q", pub.seen, want)
	}
	if c, _ := coord.Cursor(context.Background(), dispatch.BusCursor); c != 5 {
		t.Errorf("cursor %d", c)
	}
	coord = memory.NewCoordinator() // a batch larger than all events
	_ = coord.SetCursor(context.Background(), dispatch.EventsConsumer, 5)
	if n, err := (&dispatch.Bus{Feed: fd, Publisher: &batches{}, Coord: coord}).PublishDue(context.Background()); err != nil || n != len(fd)-1 {
		t.Errorf("published %d of %d in one batch, %v", n, len(fd)-1, err)
	}
}

// batches is a bus that remembers the batches.
type batches struct{ seen []string }

func (p *batches) Publish(_ context.Context, msgs []port.Message) error {
	s := ""
	for i, m := range msgs {
		if i > 0 {
			s += " "
		}
		s += fmt.Sprintf("%d/%d", m.Position, m.Index)
	}
	p.seen = append(p.seen, s)
	return nil
}

// TestBusFailure: while the bus is unavailable the cursor stays; after it
// recovers, every event is published, in order, without gaps.
func TestBusFailure(t *testing.T) {
	fd := units(1, 2, 1, 1)
	coord := memory.NewCoordinator()
	_ = coord.SetCursor(context.Background(), dispatch.EventsConsumer, 4)
	var fails atomic.Int32
	pub := &memory.Bus{Fail: func([]port.Message) error {
		if fails.Add(1) <= 3 {
			return fmt.Errorf("the broker is unavailable: %w", port.ErrPermanent)
		}
		return nil
	}}
	b := &dispatch.Bus{Feed: fd, Publisher: pub, Coord: coord, Batch: 2, Pause: func(int) time.Duration { return time.Millisecond }}
	if _, err := b.PublishDue(context.Background()); !errors.Is(err, port.ErrPermanent) {
		t.Fatalf("PublishDue with the bus unavailable: %v", err)
	}
	if c, _ := coord.Cursor(context.Background(), dispatch.BusCursor); c != 0 || len(pub.Messages()) != 0 {
		t.Fatalf("cursor %d, published %d", c, len(pub.Messages()))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = b.Run(ctx)
		close(done)
	}()
	defer func() { cancel(); <-done }()
	waitFor(t, func() bool { return len(pub.Messages()) == len(fd) })
	if got, want := ids(pub.Messages()), ids(fd.messages()); !slices.Equal(got, want) {
		t.Errorf("published %v, want %v", got, want)
	}
}

// TestNotifier: the notifications of a committed unit go to the bus after
// commit, with the number in the unit and the initiator as the recipient;
// personal arguments are null; a full queue loses notifications rather
// than delaying the command.
func TestNotifier(t *testing.T) {
	m := conformance.PersonalMachine(t)
	programs := kernel.NewPrograms()
	programs.Set(tenant, m)
	person := m.Schema().Lookup("Person")
	pub := &memory.Bus{}
	n := &dispatch.Notifier{Publisher: pub, Catalog: programs, Buffer: 2}
	k, err := kernel.New(kernel.Config{Programs: programs, Store: memory.NewStore(programs), Clock: memory.NewClock(start), Notify: n.Notify})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := k.NewID(context.Background(), tenant, person.Ref)
	a := reasonr.NewArena()
	exec := func(cmd string, args ...reasonr.Value) port.Position {
		t.Helper()
		res, err := k.Command(context.Background(), &kind.CommandRequest{Tenant: tenant, Call: reasonr.CommandCall{
			Entity: person.Ref, ID: id, Command: person.Method(cmd).Ref, Args: a.Record(0, args...), Arena: a,
			Caller: reasonr.Caller{Kind: reasonr.CallerUser, Subject: id, Actor: person.Ref}}})
		if err != nil || len(res.Errors) > 0 {
			t.Fatalf("%s: %v", cmd, err)
		}
		defer res.Release()
		return res.Position
	}
	exec("Register", a.String("+79990000000"), a.String("Anne"), a.String("Moscow")) // no notifications
	pos := exec("Call", a.String("+79990000000"), reasonr.Bool(false))
	n.Flush(context.Background())
	idJSON, _ := codecs.JSONOf(m.Schema(), codecs.Options{}).Append(nil, a, reasonr.Type{Kind: reasonr.KindID, Ref: person.Ref}, id)
	msgs := pub.Messages()
	if len(msgs) != 1 {
		t.Fatalf("%d notifications", len(msgs))
	}
	got := msgs[0]
	if got.Kind != port.MessageNotification || got.Position != pos || got.Index != 0 || got.Channel() != "notification.Person" ||
		got.Name != "calling" || got.Subject != bus.Subject(idJSON) || len(idJSON) != 38 || !got.At.Equal(start) || string(got.Data) != `{"phone":null}` {
		t.Errorf("notification %+v %s", got, got.Data)
	}
	if want := fmt.Sprintf("default/%d/n0", pos); bus.ID(&got) != want {
		t.Errorf("ce_id %s, want %s", bus.ID(&got), want)
	}
	for range 3 { // a queue of 2: the third is lost
		exec("Call", a.String("+79990000000"), reasonr.Bool(false))
	}
	n.Flush(context.Background())
	if len(pub.Messages()) != 3 {
		t.Errorf("published %d after overflow, want 3", len(pub.Messages()))
	}
}
