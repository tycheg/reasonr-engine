package porttest

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tycheg/reasonr-engine/port"
)

// person is a people row with a personal value of the subject id.
func person(id string, phone byte) port.Row {
	return port.Row{ID: id, State: []byte(`{"name":"` + id + `","phone":null}`), Sealed: []byte(`{"phone":"sealed-` + id + `"}`),
		Subjects: []port.SubjectRef{subject(id)}, Keys: []port.Scalar{[]byte{'b', phone}}}
}

func subject(id string) port.SubjectRef {
	return port.SubjectRef{Type: People.Type, ID: `"` + id + `"`}
}

// erase is the rewriter of forget: it drops the personal values and blind
// keys of a row.
func erase(_ context.Context, _ port.Tenant, _ *port.Table, r port.Row) (port.Row, bool, error) {
	r.Sealed, r.Subjects = nil, nil
	for i := range r.Keys {
		r.Keys[i] = nil
	}
	r.State = bytes.Replace(r.State, []byte(`"name":`), []byte(`"forgotten":true,"name":`), 1)
	return r, true, nil
}

// RunKeys checks port.KeyStore and the keys of a unit of work: tenant
// keys, subject keys with versions and tombstones, the key versions a
// unit used, forgetting subjects in rows, personal values in rows.
func RunKeys(t *testing.T, open Open) {
	t.Run("TenantKeys", func(t *testing.T) {
		ks := laidOut(t, open).(port.KeyStore)
		if k, err := ks.TenantKey(ctx(t), T1); k != nil || err != nil {
			t.Fatalf("no key yet: %q %v", k, err)
		}
		if k, err := ks.CreateTenantKey(ctx(t), T1, []byte("w1")); err != nil || string(k) != "w1" {
			t.Fatalf("CreateTenantKey: %q %v", k, err)
		}
		if k, err := ks.CreateTenantKey(ctx(t), T1, []byte("w2")); err != nil || string(k) != "w1" {
			t.Fatalf("the stored key wins: %q %v", k, err)
		}
		if k, err := ks.TenantKey(ctx(t), T1); string(k) != "w1" || err != nil {
			t.Fatalf("TenantKey: %q %v", k, err)
		}
		r, ok := ks.(port.TenantKeyRotator)
		if !ok {
			return
		}
		if _, err := ks.CreateTenantKey(ctx(t), T2, []byte("x1")); err != nil {
			t.Fatal(err)
		}
		all, err := r.TenantKeys(ctx(t))
		if err != nil || len(all) != 2 || string(all[T1]) != "w1" || string(all[T2]) != "x1" {
			t.Fatalf("TenantKeys: %q %v", all, err)
		}
		if ok, err := r.ReplaceTenantKey(ctx(t), T1, []byte("old"), []byte("w3")); ok || err != nil {
			t.Fatalf("a replace of a changed key: %v %v", ok, err)
		}
		if ok, err := r.ReplaceTenantKey(ctx(t), T1, []byte("w1"), []byte("w3")); !ok || err != nil {
			t.Fatalf("ReplaceTenantKey: %v %v", ok, err)
		}
		if k, _ := ks.TenantKey(ctx(t), T1); string(k) != "w3" {
			t.Fatalf("the replaced key: %q", k)
		}
	})
	t.Run("SubjectKeys", func(t *testing.T) {
		s := laidOut(t, open)
		rs, ks := s.(port.RowStore), s.(port.KeyStore)
		k, err := ks.CreateSubjectKey(ctx(t), T1, subject("ann"), []byte("a1"))
		if err != nil || k.Version != 1 || string(k.Wrapped) != "a1" || k.Subject != subject("ann") {
			t.Fatalf("CreateSubjectKey: %+v %v", k, err)
		}
		if k, err := ks.CreateSubjectKey(ctx(t), T1, subject("ann"), []byte("a2")); err != nil || k.Version != 1 || string(k.Wrapped) != "a1" {
			t.Fatalf("a live key is returned: %+v %v", k, err)
		}
		// A unit creates a key of its own: version 1, a key created by now
		// in another way is a conflict.
		u := unit(People, person("bob", 2))
		u.NewKeys = []port.WrappedKey{{Subject: subject("bob"), Version: 1, Wrapped: []byte("b1")}}
		u.Keys = []port.KeyVersion{{Subject: subject("ann"), Version: 1}}
		commit(t, rs, T1, u)
		u = unit(People, person("cid", 3))
		u.NewKeys = []port.WrappedKey{{Subject: subject("bob"), Version: 1, Wrapped: []byte("b2")}}
		if _, err := rs.Commit(ctx(t), T1, u); !errors.Is(err, port.ErrConflict) {
			t.Fatalf("a key created twice: %v", err)
		}
		got, err := ks.SubjectKeys(ctx(t), T1, []port.SubjectRef{subject("ann"), subject("bob"), subject("cid")})
		if err != nil || len(got) != 2 {
			t.Fatalf("SubjectKeys: %+v %v", got, err)
		}
		for _, k := range got {
			if want := map[string]string{subject("ann").ID: "a1", subject("bob").ID: "b1"}[k.Subject.ID]; string(k.Wrapped) != want || k.Version != 1 {
				t.Fatalf("a subject key: %+v", k)
			}
		}
		if got, _ := ks.SubjectKeys(ctx(t), T2, []port.SubjectRef{subject("ann")}); len(got) != 0 {
			t.Fatalf("the keys of another tenant: %+v", got)
		}
		r := load(t, rs, T1, People, "bob")
		if !sameJSON(r.Sealed, person("bob", 2).Sealed) || len(r.Subjects) != 1 || r.Subjects[0] != subject("bob") {
			t.Fatalf("a row with personal values: %+v", r)
		}
	})
	t.Run("Forget", func(t *testing.T) {
		s := laidOut(t, open)
		rs, ks := s.(port.RowStore), s.(port.KeyStore)
		for _, id := range []string{"ann", "bob"} {
			if _, err := ks.CreateSubjectKey(ctx(t), T1, subject(id), []byte(id)); err != nil {
				t.Fatal(err)
			}
		}
		commit(t, rs, T1, unit(People, person("ann", 1), person("bob", 2)))
		commit(t, rs, T2, unit(People, person("ann", 1)))
		u := unit(People)
		u.Forget, u.ForgetTables, u.Rewrite = []port.SubjectRef{subject("ann")}, []*port.Table{People}, erase
		commit(t, rs, T1, u)
		r := load(t, rs, T1, People, "ann")
		if r.Version != 2 || len(r.Sealed) != 0 || len(r.Subjects) != 0 || !bytes.Contains(r.State, []byte("forgotten")) {
			t.Fatalf("a forgotten row: %+v", r)
		}
		got, err := rs.Find(ctx(t), T1, &port.Query{Table: People, Conds: []port.Cond{{Column: "k_phone", Op: port.OpEq, Values: []port.Scalar{[]byte{'b', 1}}}},
			Order: []port.Order{{Column: "id"}}, Limit: -1})
		if err != nil || len(got) != 0 {
			t.Fatalf("the blind key of a forgotten row: %v %v", ids(got), err)
		}
		if r := load(t, rs, T1, People, "bob"); r.Version != 1 || len(r.Subjects) != 1 {
			t.Fatalf("another subject's row: %+v", r)
		}
		if r := load(t, rs, T2, People, "ann"); r.Version != 1 || len(r.Subjects) != 1 {
			t.Fatalf("another tenant's row: %+v", r)
		}
		keys, err := ks.SubjectKeys(ctx(t), T1, []port.SubjectRef{subject("ann")})
		if err != nil || len(keys) != 1 || keys[0].Wrapped != nil || keys[0].Version != 1 {
			t.Fatalf("a destroyed key is a tombstone: %+v %v", keys, err)
		}
		// A unit encrypted with the destroyed key is refused; a new key
		// after the tombstone has the next version.
		u = unit(People, person("cid", 3))
		u.Keys = []port.KeyVersion{{Subject: subject("ann"), Version: 1}}
		if _, err := rs.Commit(ctx(t), T1, u); !errors.Is(err, port.ErrConflict) {
			t.Fatalf("a unit with a destroyed key: %v", err)
		}
		if k, err := ks.CreateSubjectKey(ctx(t), T1, subject("ann"), []byte("ann2")); err != nil || k.Version != 2 || string(k.Wrapped) != "ann2" {
			t.Fatalf("a key after a tombstone: %+v %v", k, err)
		}
	})
}

// RunProjections checks port.AsyncProjections: rows written by position,
// deletion marks, a repeated or older position, forget, the applied position.
func RunProjections(t *testing.T, open Open) {
	ps := laidOut(t, open).(port.AsyncProjections)
	w := func(id string, sum int64) port.RowWrite {
		return port.RowWrite{Table: Totals, Key: `"` + id + `"`, Row: port.Row{ID: id, State: state("sum", int(sum)), Keys: []port.Scalar{sum}}}
	}
	del := func(id string) port.RowDelete { return port.RowDelete{Table: Totals, ID: id, Key: `"` + id + `"`} }
	apply := func(tenant port.Tenant, pos port.Position, writes []port.RowWrite, deletes []port.RowDelete) {
		t.Helper()
		must(t, ps.Apply(ctx(t), tenant, pos, writes, deletes, nil, nil, keep))
	}
	if pos, err := ps.Applied(ctx(t), T1); pos != 0 || err != nil {
		t.Fatalf("nothing applied: %d %v", pos, err)
	}
	apply(T1, 3, []port.RowWrite{w("ann", 1), w("bob", 2)}, nil)
	apply(T1, 2, []port.RowWrite{w("ann", 100)}, nil) // older: changes nothing
	if r := load(t, ps, T1, Totals, "ann"); r.Version != 3 || !sameJSON(r.State, state("sum", 1)) {
		t.Fatalf("a row is written with the version of its position: %+v", r)
	}
	apply(T1, 5, nil, []port.RowDelete{del("bob")})
	apply(T1, 4, []port.RowWrite{w("bob", 4)}, nil) // older than the deletion
	if _, ok, err := ps.Load(ctx(t), T1, Totals, "bob"); ok || err != nil {
		t.Fatalf("a deleted row stays deleted: %v %v", ok, err)
	}
	apply(T1, 6, []port.RowWrite{w("bob", 6)}, nil)
	if r := load(t, ps, T1, Totals, "bob"); r.Version != 6 {
		t.Fatalf("a row written after its deletion: %+v", r)
	}
	apply(T2, 1, nil, nil) // only the position
	if pos, err := ps.Applied(ctx(t), T1); pos != 6 || err != nil {
		t.Fatalf("Applied: %d %v", pos, err)
	}
	if pos, _ := ps.Applied(ctx(t), T2); pos != 1 {
		t.Fatalf("Applied of another tenant: %d", pos)
	}
	got, err := ps.Find(ctx(t), T1, &port.Query{Table: Totals, Conds: []port.Cond{{Column: "k_sum", Op: port.OpGe, Values: []port.Scalar{int64(1)}}},
		Order: []port.Order{{Column: "k_sum"}, {Column: "id"}}, Limit: -1})
	if err != nil || !sameIDs(got, "ann", "bob") {
		t.Fatalf("a search: %v %v", ids(got), err)
	}
	// Forget erases the rows of the subject in the tables named.
	pw := port.RowWrite{Table: People, Key: `"ann"`, Row: person("ann", 1)}
	must(t, ps.Apply(ctx(t), T1, 7, []port.RowWrite{pw}, nil, nil, nil, keep))
	must(t, ps.Apply(ctx(t), T1, 8, nil, nil, []port.SubjectRef{subject("ann")}, []*port.Table{People}, erase))
	if r := load(t, ps, T1, People, "ann"); len(r.Subjects) != 0 || !bytes.Contains(r.State, []byte("forgotten")) {
		t.Fatalf("a forgotten row: %+v", r)
	}
}

// RunCoordinator checks port.Coordinator: cursors, leases, dead letters.
func RunCoordinator(t *testing.T, open Open) {
	c := laidOut(t, open).(port.Coordinator)
	if pos, err := c.Cursor(ctx(t), "events"); pos != 0 || err != nil {
		t.Fatalf("a new cursor: %d %v", pos, err)
	}
	must(t, c.SetCursor(ctx(t), "events", 7))
	must(t, c.SetCursor(ctx(t), "bus", 3))
	if pos, err := c.Cursor(ctx(t), "events"); pos != 7 || err != nil {
		t.Fatalf("Cursor: %d %v", pos, err)
	}
	acquire := func(holder string, now time.Duration) bool {
		t.Helper()
		ok, err := c.Acquire(ctx(t), "stream", holder, Start.Add(now), time.Minute)
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		return ok
	}
	if !acquire("a", 0) || acquire("b", 30*time.Second) || !acquire("a", 50*time.Second) || acquire("b", 100*time.Second) ||
		!acquire("b", 111*time.Second) {
		t.Fatal("a lease belongs to its holder until it expires and is extended by it")
	}
	d1 := port.DeadLetter{Consumer: "bus", Position: 4, Item: -1, Tenant: T1, Reason: "no topic", At: Start}
	d2 := port.DeadLetter{Consumer: "requests", Position: 5, Item: 1, Tenant: T2, Reason: "rejected", At: Start.Add(time.Second)}
	must(t, c.Bury(ctx(t), d1))
	must(t, c.Bury(ctx(t), d2))
	got, err := c.DeadLetters(ctx(t), "bus")
	if err != nil || len(got) != 1 || got[0].Position != 4 || got[0].Item != -1 || got[0].Tenant != T1 || got[0].Reason != "no topic" ||
		!got[0].At.Equal(Start) {
		t.Fatalf("DeadLetters: %+v %v", got, err)
	}
	if got, _ := c.DeadLetters(ctx(t), ""); len(got) != 2 {
		t.Fatalf("the dead letters of every consumer: %+v", got)
	}
	must(t, c.Resurrect(ctx(t), d1))
	if got, _ := c.DeadLetters(ctx(t), ""); len(got) != 1 || got[0].Consumer != "requests" {
		t.Fatalf("a resurrected letter: %+v", got)
	}
}

// RunFeatures checks port.FeatureStore: switching on and off at levels,
// everywhere, the journal.
func RunFeatures(t *testing.T, open Open) {
	fs := laidOut(t, open).(port.FeatureStore)
	change := func(c port.FeatureChange) {
		t.Helper()
		c.By, c.At = "ops", Start.Add(time.Duration(len(c.Feature))*time.Second)
		must(t, fs.ChangeFeature(ctx(t), c))
	}
	tenant := port.FeatureScope{Tenant: T1}
	role := port.FeatureScope{Tenant: T1, Role: "admin"}
	change(port.FeatureChange{Feature: "Shop.fast", On: true, Scope: tenant})
	change(port.FeatureChange{Feature: "Shop.fast", On: true, Scope: tenant}) // again: journaled only
	change(port.FeatureChange{Feature: "Shop.fast", On: true, Scope: role})
	change(port.FeatureChange{Feature: "Shop.new", On: true})
	change(port.FeatureChange{Feature: "Shop.new", Scope: tenant}) // not on at this level
	rules, err := fs.FeatureRules(ctx(t))
	if err != nil || len(rules) != 3 {
		t.Fatalf("FeatureRules: %+v %v", rules, err)
	}
	has := func(f string, sc port.FeatureScope) bool {
		rules, _ := fs.FeatureRules(ctx(t))
		for _, r := range rules {
			if r.Feature == f && r.Scope == sc {
				return true
			}
		}
		return false
	}
	if !has("Shop.fast", tenant) || !has("Shop.fast", role) || !has("Shop.new", port.FeatureScope{}) {
		t.Fatalf("the rules: %+v", rules)
	}
	change(port.FeatureChange{Feature: "Shop.fast", Scope: role})
	if has("Shop.fast", role) || !has("Shop.fast", tenant) {
		t.Fatal("switching off a level")
	}
	change(port.FeatureChange{Feature: "Shop.fast", Everywhere: true})
	if has("Shop.fast", tenant) || !has("Shop.new", port.FeatureScope{}) {
		t.Fatal("switching off everywhere")
	}
	log, err := fs.FeatureLog(ctx(t), "Shop.fast")
	if err != nil || len(log) != 5 || !log[0].On || log[0].Scope != tenant || log[0].By != "ops" || !log[4].Everywhere {
		t.Fatalf("FeatureLog: %+v %v", log, err)
	}
	if all, _ := fs.FeatureLog(ctx(t), ""); len(all) != 7 {
		t.Fatalf("the journal of every flag: %d entries", len(all))
	}
}

// RunForgetLog checks port.ForgetLog: entries by position from a moment,
// a repeated entry.
func RunForgetLog(t *testing.T, open Open) {
	fl := laidOut(t, open).(port.ForgetLog)
	e := func(tenant port.Tenant, pos port.Position, id string) port.Forgotten {
		return port.Forgotten{Tenant: tenant, Position: pos, At: Start.Add(time.Duration(pos) * time.Minute), Subject: subject(id)}
	}
	must(t, fl.Record(ctx(t), []port.Forgotten{e(T1, 1, "ann"), e(T2, 2, "bob")}))
	must(t, fl.Record(ctx(t), []port.Forgotten{e(T1, 1, "ann"), e(T1, 3, "cid")}))
	got, err := fl.Forgotten(ctx(t), Start)
	if err != nil || len(got) != 3 || !sameForgotten(got[0], e(T1, 1, "ann")) || got[2].Position != 3 {
		t.Fatalf("Forgotten: %+v %v", got, err)
	}
	if got, _ := fl.Forgotten(ctx(t), Start.Add(2*time.Minute)); len(got) != 2 || got[0].Subject != subject("bob") {
		t.Fatalf("Forgotten since a moment: %+v", got)
	}
}

func sameForgotten(a, b port.Forgotten) bool {
	return a.Tenant == b.Tenant && a.Position == b.Position && a.At.Equal(b.At) && a.Subject == b.Subject
}
