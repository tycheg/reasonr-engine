// Package porttest holds the conformance suites of the byte-level ports
// (plan 0022, decision 5): an adapter of another module runs them in its
// tests on a live store, bus, hub or KMS. The suites lay the store out by
// hand (port.Layout) and write rows, timers, the outbox, requests, keys and
// events as bytes: no model, no compiler, no engine. What the engine
// builds on top of a store — the model's encoding, personal data, the
// kernel — is checked by the distribution's end-to-end tests.
package porttest

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/tycheg/reasonr-engine/port"
)

// The tenants of the suites.
const (
	T1 port.Tenant = "t1"
	T2 port.Tenant = "t2"
)

// Start is the start time of the suites' clock.
var Start = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// Open opens an empty store under test: a new database or schema of its
// own for each call, closed when t ends. It returns the value implementing
// the byte-level ports the store supports (port.RowStore and the others).
type Open func(t *testing.T) any

// The layout of the suites: the tables of a fictional shop.
var (
	// Orders are entities with a uuid id and a key column of every kind.
	Orders = &port.Table{Type: "Shop.Order", ID: port.ColUUID, Ordered: true,
		Keys: []port.Column{
			{Name: "k_customer", Kind: port.ColText},
			{Name: "k_total", Kind: port.ColInt},
			{Name: "k_placed", Kind: port.ColNanos},
			{Name: "k_price", Kind: port.ColNumeric},
			{Name: "k_rate", Kind: port.ColFloat},
			{Name: "k_paid", Kind: port.ColBool},
			{Name: "k_code", Kind: port.ColBytes},
			{Name: "k_route", Kind: port.ColOpaque},
		},
		Indexes: []port.Index{
			{Name: "customer", Columns: []string{"k_customer", "k_total"}, Desc: []bool{false, true}},
			{Name: "code", Columns: []string{"k_code"}, Desc: []bool{false}, Unique: true},
		}}
	// People are entities with personal values: a text id and a blind key.
	People = &port.Table{Type: "Shop.Person", ID: port.ColText, Personal: true, Ordered: true,
		Keys:    []port.Column{{Name: "k_phone", Kind: port.ColBlind, Field: "phone"}},
		Indexes: []port.Index{{Name: "phone", Columns: []string{"k_phone"}, Desc: []bool{false}, Unique: true}}}
	// Counters have a sequence id.
	Counters = &port.Table{Type: "Shop.Counter", ID: port.ColInt, Ordered: true}
	// Totals is a projection.
	Totals = &port.Table{Type: "Shop.Totals", ID: port.ColText, Projection: true, Ordered: true,
		Keys:    []port.Column{{Name: "k_sum", Kind: port.ColInt}},
		Indexes: []port.Index{{Name: "sum", Columns: []string{"k_sum"}, Desc: []bool{false}}}}
)

// Layout is the layout of the suites' model.
func Layout() *port.Layout {
	return &port.Layout{Model: "base", Tables: []port.Table{*Orders, *People, *Counters, *Totals},
		Schema: []byte(`{"model":"shop","version":1}`)}
}

// table returns the table of type typ of the layout l.
func table(l *port.Layout, typ string) *port.Table {
	for i := range l.Tables {
		if l.Tables[i].Type == typ {
			return &l.Tables[i]
		}
	}
	panic("porttest: no table " + typ)
}

// RunStore runs the suites of every byte-level store port s implements.
func RunStore(t *testing.T, open Open) {
	probe := open(t)
	run := func(name string, ok bool, f func(t *testing.T, open Open)) {
		if ok {
			t.Run(name, func(t *testing.T) { f(t, open) })
		}
	}
	_, rows := probe.(port.RowStore)
	_, schemas := probe.(port.Schemas)
	_, timers := probe.(port.TimerStore)
	_, outbox := probe.(port.Outbox)
	_, requests := probe.(port.Requests)
	_, async := probe.(port.AsyncProjections)
	_, events := probe.(port.EventLog)
	_, keys := probe.(port.KeyStore)
	_, coord := probe.(port.Coordinator)
	_, features := probe.(port.FeatureStore)
	_, forget := probe.(port.ForgetLog)
	_, transfer := probe.(port.Transfer)
	_, sink := probe.(port.RowSink)
	run("Schemas", schemas, RunSchemas)
	run("Rows", rows && schemas, RunRows)
	run("Find", rows && schemas, RunFind)
	run("Timers", rows && schemas && timers, RunTimers)
	run("Outbox", rows && schemas && outbox, RunOutbox)
	run("Requests", rows && schemas && requests, RunRequests)
	run("Projections", async && schemas, RunProjections)
	run("Events", events && schemas, RunEvents)
	run("Keys", rows && schemas && keys, RunKeys)
	run("Transfer", rows && schemas && transfer && timers, RunTransfer)
	run("RowSink", rows && schemas && sink, RunRowSink)
	run("Coordinator", coord, RunCoordinator)
	run("Features", features, RunFeatures)
	run("ForgetLog", forget, RunForgetLog)
}

// laidOut opens a store and lays the suites' model out in it for T1 and T2.
func laidOut(t *testing.T, open Open) any {
	t.Helper()
	s := open(t)
	if sc, ok := s.(port.Schemas); ok {
		if err := sc.EnsureSchema(ctx(t), Layout(), []port.Tenant{T1, T2}, keep); err != nil {
			t.Fatalf("EnsureSchema: %v", err)
		}
	}
	return s
}

// keep is a rewriter that changes nothing.
func keep(_ context.Context, _ port.Tenant, _ *port.Table, r port.Row) (port.Row, bool, error) {
	return r, false, nil
}

func ctx(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	return c
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// uuid is a uuid with the bytes b at its start.
func uuid(b ...byte) [16]byte {
	var u [16]byte
	copy(u[:], b)
	return u
}

// order is an orders row: id n, customer c, total total; the other keys are
// derived from n.
func order(n byte, c string, total int64) port.Row {
	return port.Row{ID: uuid(n), State: state("order", int(n)),
		Keys: []port.Scalar{c, total, fmt.Sprint(Start.UnixNano() + int64(n)), fmt.Sprintf("%d.5", n), float64(n) / 4, n%2 == 0,
			[]byte{'c', n}, []byte{'r', n % 3}}}
}

// state is a canonical JSON state.
func state(kind string, n int) []byte { return []byte(fmt.Sprintf(`{"%s":%d}`, kind, n)) }

// unit is a unit of work that writes rows of t (new ones: version 0) and
// records an outbox entry.
func unit(t *port.Table, rows ...port.Row) *port.UnitRecord {
	u := &port.UnitRecord{Outbox: []byte(`{}`), Now: Start}
	for _, r := range rows {
		u.Writes = append(u.Writes, port.RowWrite{Table: t, Row: r})
	}
	return u
}

// commit commits u and returns its position.
func commit(t *testing.T, s port.RowStore, tenant port.Tenant, u *port.UnitRecord) port.Position {
	t.Helper()
	pos, err := s.Commit(ctx(t), tenant, u)
	if err != nil {
		t.Fatalf("Commit: %v", err)
	}
	return pos
}

// load reads a row that must exist.
func load(t *testing.T, s port.RowReader, tenant port.Tenant, tb *port.Table, id port.Scalar) port.Row {
	t.Helper()
	r, ok, err := s.Load(ctx(t), tenant, tb, id)
	if err != nil || !ok {
		t.Fatalf("Load %s %v: %v, exists %v", tb.Type, id, err, ok)
	}
	return r
}

// sameScalar compares key or id values as the store gives them back.
func sameScalar(a, b port.Scalar) bool {
	switch x := a.(type) {
	case []byte:
		y, ok := b.([]byte)
		return ok && bytes.Equal(x, y)
	}
	return a == b
}

// ids are the ids of rows.
func ids(rows []port.Row) []port.Scalar {
	out := make([]port.Scalar, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}

// sameIDs reports whether the ids are want in this order.
func sameIDs(got []port.Row, want ...port.Scalar) bool {
	return slices.EqualFunc(ids(got), want, sameScalar)
}
