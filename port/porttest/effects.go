package porttest

import (
	"errors"
	"testing"
	"time"

	"github.com/tycheg/reasonr-engine/port"
)

// timer is a timer record id n firing at Start + at with the offset offset.
func timer(n byte, at, offset time.Duration) port.TimerRecord {
	return port.TimerRecord{ID: uuid(0xf0, n), At: Start.Add(at), Offset: offset, Entity: Orders.Type, EntityID: []byte(`"o1"`),
		Command: "expire", Args: state("timer", int(n)), Sealed: []byte(`{"phone":"sealed"}`)}
}

// lease leases the timers due at Start + now for a minute.
func lease(t *testing.T, s port.TimerStore, tenant port.Tenant, now time.Duration, scenario bool) []port.LeasedTimerRecord {
	t.Helper()
	got, err := s.Lease(ctx(t), port.LeaseRequest{Tenant: tenant, Now: Start.Add(now), Limit: 10, TTL: time.Minute, Scenario: scenario})
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}
	return got
}

// timerIDs are the ids of leased timers.
func timerIDs(ts []port.LeasedTimerRecord) []port.Scalar {
	out := make([]port.Scalar, len(ts))
	for i, lt := range ts {
		out[i] = lt.ID
	}
	return out
}

func sameTimers(got []port.LeasedTimerRecord, want ...byte) bool {
	if len(got) != len(want) {
		return false
	}
	for i, n := range want {
		if got[i].ID != uuid(0xf0, n) {
			return false
		}
	}
	return true
}

// RunTimers checks the timers of a unit of work and port.TimerStore:
// leases in the order of the moment, retries, dead timers, the fired timer
// of a unit, the scenario clock.
func RunTimers(t *testing.T, open Open) {
	s := laidOut(t, open)
	rs, ts := s.(port.RowStore), s.(port.TimerStore)
	u := unit(Orders)
	u.Timers = []port.TimerRecord{timer(2, 2*time.Minute, 0), timer(1, time.Minute, 0), timer(3, 10*time.Minute, 0)}
	commit(t, rs, T1, u)
	u = unit(Orders)
	u.Timers = []port.TimerRecord{timer(4, 90*time.Second, 0)}
	commit(t, rs, T2, u)

	if got := lease(t, ts, "", 30*time.Second, false); len(got) != 0 {
		t.Fatalf("nothing is due: %x", timerIDs(got))
	}
	got := lease(t, ts, "", 100*time.Second, false)
	if !sameTimers(got, 1, 4) {
		t.Fatalf("the due timers in the order of the moment: %x", timerIDs(got))
	}
	want := timer(1, time.Minute, 0)
	if lt := got[0]; lt.Tenant != T1 || !lt.At.Equal(want.At) || lt.Offset != 0 || lt.Entity != want.Entity || !sameJSON(lt.EntityID, want.EntityID) ||
		lt.Command != want.Command || !sameJSON(lt.Args, want.Args) || !sameJSON(lt.Sealed, want.Sealed) || lt.Attempts != 1 {
		t.Fatalf("a leased timer: %+v", lt)
	}
	if got := lease(t, ts, "", 110*time.Second, false); len(got) != 0 {
		t.Fatalf("leased timers are not leased again: %x", timerIDs(got))
	}
	if got := lease(t, ts, T1, 170*time.Second, false); !sameTimers(got, 1, 2) || got[0].Attempts != 2 {
		t.Fatalf("an expired lease, a tenant: %x", timerIDs(got))
	}
	must(t, ts.Retry(ctx(t), T1, uuid(0xf0, 1), Start.Add(20*time.Minute)))
	must(t, ts.Dead(ctx(t), T1, uuid(0xf0, 2), "the command fails"))
	if got := lease(t, ts, T1, 15*time.Minute, false); !sameTimers(got, 3) {
		t.Fatalf("a retried timer waits, a dead one is gone: %x", timerIDs(got))
	}
	if got := lease(t, ts, T1, 20*time.Minute, false); !sameTimers(got, 1, 3) || got[0].Attempts != 3 {
		t.Fatalf("a retried timer, an expired lease: %x", timerIDs(got))
	}
	fired := uuid(0xf0, 1)
	u = unit(Orders, order(1, "ann", 1))
	u.Fired = &fired
	commit(t, rs, T1, u)
	u = unit(Orders, order(2, "bob", 1))
	u.Fired = &fired
	if _, err := rs.Commit(ctx(t), T1, u); !errors.Is(err, port.ErrTimerGone) {
		t.Fatalf("a timer fired twice: %v", err)
	}
	if _, ok, _ := rs.Load(ctx(t), T1, Orders, uuid(2)); ok {
		t.Fatal("a unit of a gone timer wrote a row")
	}
	u = unit(Orders)
	u.DeleteTimers = [][16]byte{uuid(0xf0, 3)}
	commit(t, rs, T1, u)
	if got := lease(t, ts, T1, time.Hour, false); len(got) != 0 {
		t.Fatalf("fired and deleted timers: %x", timerIDs(got))
	}

	// The scenario clock: a timer set an hour ahead of the real clock
	// fires by its scenario moment in a scenario, by the real one otherwise.
	u = unit(Orders)
	u.Timers = []port.TimerRecord{timer(5, 2*time.Hour, time.Hour), timer(6, 2*time.Hour+30*time.Minute, 0)}
	commit(t, rs, T1, u)
	if got := lease(t, ts, T1, 2*time.Hour+20*time.Minute, true); len(got) != 0 {
		t.Fatalf("in a scenario the offset timer is not due before its scenario moment: %x", timerIDs(got))
	}
	if got := lease(t, ts, T1, 3*time.Hour+10*time.Minute, true); !sameTimers(got, 6, 5) || got[1].Offset != time.Hour {
		t.Fatalf("the scenario order: %x", timerIDs(got))
	}
}

// RunOutbox checks port.Outbox: units in the order of positions, from a
// position, a limit, waiting for the next unit.
func RunOutbox(t *testing.T, open Open) {
	s := laidOut(t, open)
	rs, ob := s.(port.RowStore), s.(port.Outbox)
	var pos []port.Position
	for i, tenant := range []port.Tenant{T1, T2, T1} {
		u := unit(Orders)
		u.Outbox = state("unit", i)
		pos = append(pos, commit(t, rs, tenant, u))
	}
	got, err := ob.Read(ctx(t), 0, 10)
	if err != nil || len(got) != 3 {
		t.Fatalf("Read: %+v %v", got, err)
	}
	for i, r := range got {
		if r.Position != pos[i] || r.Tenant != []port.Tenant{T1, T2, T1}[i] || !sameJSON(r.Payload, state("unit", i)) {
			t.Fatalf("unit %d: %+v", i, r)
		}
	}
	if got, err := ob.Read(ctx(t), pos[0], 1); err != nil || len(got) != 1 || got[0].Position != pos[1] {
		t.Fatalf("Read from a position with a limit: %+v %v", got, err)
	}
	must(t, ob.Wait(ctx(t), pos[1])) // there is a later unit
	done := make(chan error, 1)
	go func() { done <- ob.Wait(ctx(t), pos[2]) }()
	select {
	case err := <-done:
		t.Fatalf("Wait returned without a new unit: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	commit(t, rs, T2, unit(Orders))
	select {
	case err := <-done:
		must(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Wait did not return after a new unit")
	}
}

// RunRequests checks the external requests of a unit of work and
// port.Requests: leases, retries, the deadline timer, done requests.
func RunRequests(t *testing.T, open Open) {
	s := laidOut(t, open)
	rs, q := s.(port.RowStore), s.(port.Requests)
	deadline := uuid(0xf0, 9)
	u := unit(Orders)
	u.Timers = []port.TimerRecord{timer(9, time.Hour, 0)}
	u.Requests = []port.RequestRecord{
		{Key: uuid(0xe0, 1), System: "Shop.Bank", Request: "charge", Args: state("amount", 5), Sealed: []byte(`{"card":"sealed"}`)},
		{Key: uuid(0xe0, 2), System: "Shop.Mail", Request: "send", Args: state("letter", 1), Timer: &deadline},
	}
	pos := commit(t, rs, T1, u)
	leaseAt := func(now time.Duration) []port.QueuedRequest {
		t.Helper()
		got, err := q.LeaseRequests(ctx(t), Start.Add(now), 10, time.Minute)
		if err != nil {
			t.Fatalf("LeaseRequests: %v", err)
		}
		return got
	}
	keys := func(rs []port.QueuedRequest) []port.Scalar {
		out := make([]port.Scalar, len(rs))
		for i, r := range rs {
			out[i] = r.Request.Key
		}
		return out
	}
	got := leaseAt(0)
	if len(got) != 2 || got[0].Request.Key != uuid(0xe0, 1) || got[1].Request.Key != uuid(0xe0, 2) {
		t.Fatalf("the requests of a unit in order: %x", keys(got))
	}
	r1, r2 := got[0], got[1]
	if r1.Tenant != T1 || r1.Position != pos || !r1.At.Equal(Start) || r1.Attempts != 1 || r1.Request.System != "Shop.Bank" ||
		r1.Request.Request != "charge" || !sameJSON(r1.Request.Args, state("amount", 5)) || !sameJSON(r1.Request.Sealed, []byte(`{"card":"sealed"}`)) ||
		!r1.Deadline.IsZero() || r1.TimerGone {
		t.Fatalf("a request: %+v", r1)
	}
	if !r2.Deadline.Equal(Start.Add(time.Hour)) || r2.TimerGone {
		t.Fatalf("a request with a deadline: %+v", r2)
	}
	if got := leaseAt(30 * time.Second); len(got) != 0 {
		t.Fatalf("leased requests are not leased again: %x", keys(got))
	}
	must(t, q.RetryRequest(ctx(t), T1, uuid(0xe0, 1), Start.Add(5*time.Minute), "the bank is down"))
	if got := leaseAt(2 * time.Minute); len(got) != 1 || got[0].Request.Key != uuid(0xe0, 2) || got[0].Attempts != 2 {
		t.Fatalf("an expired lease: %x", keys(got))
	}
	u = unit(Orders)
	u.DeleteTimers = [][16]byte{deadline}
	commit(t, rs, T1, u)
	got = leaseAt(5 * time.Minute)
	byKey := map[[16]byte]port.QueuedRequest{}
	for _, r := range got {
		byKey[r.Request.Key] = r
	}
	if r1, r2 := byKey[uuid(0xe0, 1)], byKey[uuid(0xe0, 2)]; len(got) != 2 || r1.Attempts != 2 || r1.TimerGone || !r2.TimerGone {
		t.Fatalf("a retried request, a request whose timer is gone: %+v", got)
	}
	must(t, q.RequestDone(ctx(t), T1, uuid(0xe0, 1)))
	must(t, q.RequestDone(ctx(t), T1, uuid(0xe0, 2)))
	if got := leaseAt(time.Hour); len(got) != 0 {
		t.Fatalf("done requests: %x", keys(got))
	}
}

// event is an event record of the unit at pos.
func event(tenant port.Tenant, pos port.Position, seq int, name string) port.EventRecord {
	return port.EventRecord{Tenant: tenant, Position: pos, Seq: seq, Type: Orders.Type, ID: []byte(`"o1"`), Version: uint64(seq + 1),
		Name: name, Args: state(name, seq), At: Start.Add(time.Duration(pos) * time.Second)}
}

// RunEvents checks port.EventLog and port.EventFeed (if the store has
// it): events by position, within a unit by entity and event.
func RunEvents(t *testing.T, open Open) {
	s := laidOut(t, open)
	log := s.(port.EventLog)
	all := []port.EventRecord{event(T1, 1, 0, "placed"), event(T1, 1, 1, "paid"), event(T2, 2, 0, "placed"), event(T1, 3, 0, "shipped")}
	must(t, log.Append(ctx(t), all[:2]))
	must(t, log.Append(ctx(t), all[2:]))
	must(t, log.Append(ctx(t), all[3:])) // a repeated append changes nothing
	feed, ok := s.(port.EventFeed)
	if !ok {
		return
	}
	read := func(after, upto port.Position, limit int) []port.EventRecord {
		t.Helper()
		got, err := feed.ReadEvents(ctx(t), after, upto, limit)
		if err != nil {
			t.Fatalf("ReadEvents: %v", err)
		}
		return got
	}
	got := read(0, 3, 0)
	if len(got) != len(all) {
		t.Fatalf("every event: %+v", got)
	}
	for i, e := range got {
		w := all[i]
		if e.Tenant != w.Tenant || e.Position != w.Position || e.Seq != w.Seq || e.Type != w.Type || !sameJSON(e.ID, w.ID) ||
			e.Name != w.Name || !sameJSON(e.Args, w.Args) || !e.At.Equal(w.At) {
			t.Fatalf("event %d: %+v", i, e)
		}
	}
	if got := read(1, 2, 0); len(got) != 1 || got[0].Position != 2 {
		t.Fatalf("the events in (1, 2]: %+v", got)
	}
	if got := read(0, 3, 1); len(got) != 1 || got[0].Name != "placed" || got[0].Tenant != T1 {
		t.Fatalf("a limit: %+v", got)
	}
}

// RunTransfer checks port.Transfer: the timers and counters of a tenant
// are exported, the counters imported without lowering them, the timers
// cleared.
func RunTransfer(t *testing.T, open Open) {
	s := laidOut(t, open)
	rs, tr := s.(port.RowStore), s.(port.Transfer)
	u := unit(Orders)
	u.Timers = []port.TimerRecord{timer(1, time.Minute, time.Hour)}
	commit(t, rs, T1, u)
	if _, err := rs.NextSequence(ctx(t), T1, Counters.Type, 5); err != nil {
		t.Fatal(err)
	}
	ex, err := tr.ExportTimers(ctx(t), T1)
	if err != nil || len(ex.Timers) != 1 || ex.Sequences[Counters.Type] != 5 {
		t.Fatalf("ExportTimers: %+v %v", ex, err)
	}
	want := timer(1, time.Minute, time.Hour)
	if got := ex.Timers[0]; got.ID != want.ID || !got.At.Equal(want.At) || got.Offset != want.Offset || got.Command != want.Command ||
		!sameJSON(got.Args, want.Args) {
		t.Fatalf("an exported timer: %+v", got)
	}
	must(t, tr.ImportSequences(ctx(t), T2, map[string]int64{Counters.Type: 7}))
	must(t, tr.ImportSequences(ctx(t), T2, map[string]int64{Counters.Type: 3}))
	if n, err := rs.NextSequence(ctx(t), T2, Counters.Type, 1); err != nil || n != 8 {
		t.Fatalf("the next number after an import: %d %v", n, err)
	}
	must(t, tr.ClearTimers(ctx(t), T1))
	if got := lease(t, s.(port.TimerStore), T1, time.Hour, false); len(got) != 0 {
		t.Fatalf("cleared timers: %x", timerIDs(got))
	}
}

// RunRowSink checks port.RowSink: a row is written only with a greater
// version, its key columns searchable.
func RunRowSink(t *testing.T, open Open) {
	s := laidOut(t, open)
	rs, sink := s.(port.RowStore), s.(port.RowSink)
	put := func(version uint64, st []byte) {
		t.Helper()
		r := order(1, "ann", 10)
		r.Version, r.State = version, st
		must(t, sink.PutRows(ctx(t), T1, []port.RowWrite{{Table: Orders, Row: r}}))
	}
	put(3, state("v", 3))
	put(2, state("v", 2))
	if r := load(t, rs, T1, Orders, uuid(1)); r.Version != 3 || !sameJSON(r.State, state("v", 3)) {
		t.Fatalf("an older version is not written: %+v", r)
	}
	put(5, state("v", 5))
	if r := load(t, rs, T1, Orders, uuid(1)); r.Version != 5 {
		t.Fatalf("a newer version: %+v", r)
	}
	got, err := rs.Find(ctx(t), T1, &port.Query{Table: Orders, Conds: []port.Cond{{Column: "k_customer", Op: port.OpEq, Values: []port.Scalar{"ann"}}},
		Order: []port.Order{{Column: "id"}}, Limit: -1})
	if err != nil || !sameIDs(got, uuid(1)) {
		t.Fatalf("a search: %v %v", ids(got), err)
	}
}
