package porttest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/tycheg/reasonr-engine/port"
)

// RunSchemas checks port.Schemas: the schema written last, tables and
// key columns added to a laid-out store, the rows rewritten.
func RunSchemas(t *testing.T, open Open) {
	t.Run("LastSchema", func(t *testing.T) {
		s := open(t).(port.Schemas)
		if got, err := s.LastSchema(ctx(t), "base"); err != nil || got != nil {
			t.Fatalf("a new store: %q %v", got, err)
		}
		l := Layout()
		must(t, s.EnsureSchema(ctx(t), l, []port.Tenant{T1}, keep))
		must(t, s.EnsureSchema(ctx(t), l, []port.Tenant{T1}, keep))
		if got, err := s.LastSchema(ctx(t), "base"); err != nil || !sameJSON(got, l.Schema) {
			t.Fatalf("LastSchema: %s %v", got, err)
		}
		if got, err := s.LastSchema(ctx(t), "ext"); err != nil || got != nil {
			t.Fatalf("another model: %q %v", got, err)
		}
		l.Schema = []byte(`{"model":"shop","version":2}`)
		must(t, s.EnsureSchema(ctx(t), l, []port.Tenant{T1}, keep))
		if got, _ := s.LastSchema(ctx(t), "base"); !sameJSON(got, l.Schema) {
			t.Fatalf("the next schema: %s", got)
		}
	})
	t.Run("AddedKeys", func(t *testing.T) {
		s := laidOut(t, open)
		rs := s.(port.RowStore)
		commit(t, rs, T1, unit(Orders, order(1, "ann", 10), order(2, "bob", 20)))
		commit(t, rs, T2, unit(Orders, order(1, "cid", 30)))
		// A new key column: the store has the rows of the tenants filled.
		l := Layout()
		orders := table(l, Orders.Type)
		orders.Keys = append(orders.Keys, port.Column{Name: "k_extra", Kind: port.ColInt})
		orders.Indexes = append(orders.Indexes, port.Index{Name: "extra", Columns: []string{"k_extra"}, Desc: []bool{false}})
		l.Schema = []byte(`{"model":"shop","version":2}`)
		seen := map[port.Tenant]int{}
		fill := func(_ context.Context, tenant port.Tenant, tb *port.Table, r port.Row) (port.Row, bool, error) {
			if tb.Type != Orders.Type {
				return r, false, nil
			}
			seen[tenant]++
			if len(r.Keys) != len(orders.Keys) {
				return r, false, errors.New("the row has no place for the new key column")
			}
			r.Keys[len(r.Keys)-1] = int64(42)
			return r, true, nil
		}
		must(t, s.(port.Schemas).EnsureSchema(ctx(t), l, []port.Tenant{T1, T2}, fill))
		if seen[T1] != 2 || seen[T2] != 1 {
			t.Fatalf("the rows rewritten by tenant: %v", seen)
		}
		got, err := rs.Find(ctx(t), T1, &port.Query{Table: orders, Conds: []port.Cond{{Column: "k_extra", Op: port.OpEq, Values: []port.Scalar{int64(42)}}},
			Order: []port.Order{{Column: "id"}}, Limit: -1})
		if err != nil || !sameIDs(got, uuid(1), uuid(2)) {
			t.Fatalf("a search by the new key column: %v %v", ids(got), err)
		}
	})
	t.Run("Rewrite", func(t *testing.T) {
		s := laidOut(t, open)
		rs := s.(port.RowStore)
		commit(t, rs, T1, unit(Orders, order(1, "ann", 10)))
		l := Layout()
		l.Rewrite = []string{Orders.Type}
		l.Schema = []byte(`{"model":"shop","version":2}`)
		rewrite := func(_ context.Context, _ port.Tenant, tb *port.Table, r port.Row) (port.Row, bool, error) {
			if tb.Type != Orders.Type {
				return r, false, nil
			}
			r.State = []byte(`{"order":1,"rewritten":true}`)
			return r, true, nil
		}
		must(t, s.(port.Schemas).EnsureSchema(ctx(t), l, []port.Tenant{T1}, rewrite))
		if r := load(t, rs, T1, Orders, uuid(1)); !sameJSON(r.State, []byte(`{"order":1,"rewritten":true}`)) {
			t.Fatalf("the rewritten state: %s", r.State)
		}
	})
}

// RunRows checks the unit of work of port.RowStore: versions, uniqueness,
// synchronous projections, deletions, sequences and responses by key.
func RunRows(t *testing.T, open Open) {
	t.Run("Versions", func(t *testing.T) {
		s := laidOut(t, open).(port.RowStore)
		p1 := commit(t, s, T1, unit(Orders, order(1, "ann", 10), order(2, "bob", 20)))
		p2 := commit(t, s, T2, unit(Orders, order(1, "cid", 30)))
		if p1 == 0 || p2 <= p1 {
			t.Fatalf("positions %d, %d: they grow from 1", p1, p2)
		}
		r := load(t, s, T1, Orders, uuid(1))
		if r.Version != 1 || !sameJSON(r.State, state("order", 1)) || !sameScalar(r.ID, uuid(1)) {
			t.Fatalf("a new row: %+v", r)
		}
		if _, ok, err := s.Load(ctx(t), T1, Orders, uuid(9)); ok || err != nil {
			t.Fatalf("a missing row: %v %v", ok, err)
		}
		if _, ok, err := s.Load(ctx(t), T2, Orders, uuid(2)); ok || err != nil {
			t.Fatalf("a row of another tenant: %v %v", ok, err)
		}
		next := order(1, "ann", 11)
		next.Version, next.State = 1, state("order", 100)
		commit(t, s, T1, unit(Orders, next))
		if r := load(t, s, T1, Orders, uuid(1)); r.Version != 2 || !sameJSON(r.State, state("order", 100)) {
			t.Fatalf("an updated row: %+v", r)
		}
		// A stale version, a row created twice: the whole unit is refused.
		other := order(3, "dan", 1)
		for _, w := range []port.Row{next, order(2, "bob", 20)} {
			if _, err := s.Commit(ctx(t), T1, unit(Orders, other, w)); !errors.Is(err, port.ErrConflict) {
				t.Fatalf("a conflict: %v", err)
			}
		}
		if _, ok, _ := s.Load(ctx(t), T1, Orders, uuid(3)); ok {
			t.Fatal("a refused unit wrote a row")
		}
	})
	t.Run("Unique", func(t *testing.T) {
		s := laidOut(t, open).(port.RowStore)
		if !s.Capabilities().Unique {
			t.Skip("no unique indexes")
		}
		commit(t, s, T1, unit(Orders, order(1, "ann", 10)))
		dup := order(2, "bob", 20)
		dup.Keys[6] = []byte{'c', 1}
		_, err := s.Commit(ctx(t), T1, unit(Orders, order(3, "cid", 1), dup))
		var uv *port.UniqueKeyViolation
		if !errors.As(err, &uv) || uv.Type != Orders.Type || uv.Index != "code" {
			t.Fatalf("a duplicate unique key: %v", err)
		}
		if _, ok, _ := s.Load(ctx(t), T1, Orders, uuid(3)); ok {
			t.Fatal("a refused unit wrote a row")
		}
		commit(t, s, T2, unit(Orders, dup)) // another tenant
		free := order(4, "eve", 1)
		free.Keys[6] = nil
		other := order(5, "fay", 1)
		other.Keys[6] = nil
		commit(t, s, T1, unit(Orders, free, other)) // none is not a key value
	})
	t.Run("Projections", func(t *testing.T) {
		s := laidOut(t, open).(port.RowStore)
		w := port.RowWrite{Table: Totals, Projection: true, Key: `"ann"`,
			Row: port.Row{ID: "ann", State: state("sum", 1), Keys: []port.Scalar{int64(1)}}}
		u := &port.UnitRecord{Writes: []port.RowWrite{w}, Outbox: []byte(`{}`), Now: Start}
		commit(t, s, T1, u)
		u.Writes[0].Row.State = state("sum", 2)
		commit(t, s, T1, u) // no version check: the stored version + 1
		if r := load(t, s, T1, Totals, "ann"); r.Version != 2 || !sameJSON(r.State, state("sum", 2)) {
			t.Fatalf("a synchronous projection row: %+v", r)
		}
		commit(t, s, T1, &port.UnitRecord{Deletes: []port.RowDelete{{Table: Totals, ID: "ann", Key: `"ann"`}}, Outbox: []byte(`{}`), Now: Start})
		if _, ok, err := s.Load(ctx(t), T1, Totals, "ann"); ok || err != nil {
			t.Fatalf("a deleted row: %v %v", ok, err)
		}
	})
	t.Run("Rejected", func(t *testing.T) {
		s := laidOut(t, open).(port.RowStore)
		u := unit(Orders, order(1, "ann", 10))
		u.Outbox = nil // no work: neither rows nor a position
		if pos := commit(t, s, T1, u); pos != 0 {
			t.Fatalf("the position of a unit without work: %d", pos)
		}
		if _, ok, _ := s.Load(ctx(t), T1, Orders, uuid(1)); ok {
			t.Fatal("a unit without work wrote a row")
		}
	})
	t.Run("Sequences", func(t *testing.T) {
		s := laidOut(t, open).(port.RowStore)
		for _, c := range []struct {
			tenant port.Tenant
			n      int
			first  int64
		}{{T1, 3, 1}, {T1, 1, 4}, {T2, 2, 1}, {T1, 2, 5}} {
			got, err := s.NextSequence(ctx(t), c.tenant, Counters.Type, c.n)
			if err != nil || got != c.first {
				t.Fatalf("NextSequence(%s, %d) = %d, %v; want %d", c.tenant, c.n, got, err, c.first)
			}
		}
	})
	t.Run("Idempotency", func(t *testing.T) {
		s := laidOut(t, open).(port.RowStore)
		expires := Start.Add(time.Hour)
		keyed := func(n byte, now time.Time) *port.UnitRecord {
			u := unit(Orders, order(n, "ann", int64(n)))
			u.Idempotency, u.Expires, u.Now = "k1", expires, now
			u.Result = func(pos port.Position) ([]byte, error) { return json.Marshal(map[string]any{"position": pos, "n": n}) }
			return u
		}
		pos := commit(t, s, T1, keyed(1, Start))
		want, _ := json.Marshal(map[string]any{"position": pos, "n": 1})
		if got, err := s.Remembered(ctx(t), T1, "k1", Start.Add(time.Minute)); err != nil || !sameJSON(got, want) {
			t.Fatalf("the remembered response: %s %v", got, err)
		}
		for _, c := range []struct {
			tenant port.Tenant
			now    time.Time
		}{{T2, Start}, {T1, expires.Add(time.Second)}} {
			if got, err := s.Remembered(ctx(t), c.tenant, "k1", c.now); got != nil || err != nil {
				t.Fatalf("Remembered(%s, %v): %s %v", c.tenant, c.now, got, err)
			}
		}
		if _, err := s.Commit(ctx(t), T1, keyed(2, Start.Add(time.Minute))); !errors.Is(err, port.ErrDuplicateKey) {
			t.Fatalf("a repeated key: %v", err)
		}
		if _, ok, _ := s.Load(ctx(t), T1, Orders, uuid(2)); ok {
			t.Fatal("a unit with a repeated key wrote a row")
		}
		commit(t, s, T2, keyed(3, Start))                    // another tenant
		commit(t, s, T1, keyed(4, expires.Add(time.Second))) // an expired key is free
		rejected := keyed(5, expires.Add(time.Minute))
		rejected.Idempotency, rejected.Outbox, rejected.Expires = "k2", nil, expires.Add(time.Hour)
		commit(t, s, T1, rejected) // a rejected command keeps its response too
		if got, _ := s.Remembered(ctx(t), T1, "k2", expires.Add(time.Minute)); got == nil {
			t.Fatal("the response of a rejected command is not remembered")
		}
	})
}

// sameJSON reports whether a and b are the same JSON value.
func sameJSON(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
