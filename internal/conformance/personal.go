package conformance

import (
	"bytes"
	"context"
	"strings"
	"testing"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// RunPersonal is the personal data conformance set (RFC §20): encryption in
// the store, search and uniqueness by blind indexes, forget and its race
// with a write; timers, external requests and responses by key whose
// subject is forgotten; the forget journal. The model is testdata/pii.
func RunPersonal(t *testing.T, f Factory) {
	for _, c := range []struct {
		name string
		run  func(*testing.T, Factory)
	}{
		{"Stored", testPersonalStored}, {"Forget", testPersonalForget}, {"ForgetFailed", testPersonalForgetFailed},
		{"Rebirth", testPersonalRebirth}, {"Tenants", testPersonalTenants}, {"Race", testPersonalRace},
		{"ApplyForget", testPersonalApplyForget}, {"StaleKey", testPersonalStaleKey},
		{"Timers", testPersonalTimers}, {"Replay", testPersonalReplay}, {"Requests", testPersonalRequests},
		{"ForgetLog", testPersonalForgetLog}, {"Journal", testPersonalJournal},
		{"Bus", testPersonalBus}, {"Broadcast", testPersonalBroadcast}, {"Reseal", testPersonalReseal},
		{"Separate", testPersonalSeparate}, {"Rotate", testPersonalRotate},
	} {
		t.Run(c.name, func(t *testing.T) { c.run(t, f) })
	}
}

// Markers: plaintext that must not appear in the store.
const (
	phoneA = "+79990000001"
	phoneB = "+79990000002"
	nameA  = "Anne-marker"
	nameB  = "Bob-marker"
	cityA  = "City-plain" // not personal: appears as is
)

var support = reasonr.Caller{Kind: reasonr.CallerUser, Roles: []string{"support"}}

func newPersonal(t *testing.T, sf Factory) *fixture {
	return newFixture(t, sf, options{machine: personal.get(t)})
}

func (f *fixture) register(p reasonr.Value, phone, name string) []string {
	f.t.Helper()
	return f.exec(actor(p), p, "Register", f.a.String(phone), f.a.String(name), f.a.String(cityA))
}

// text is the string of a record field or "none".
func (f *fixture) text(rec reasonr.Value, t, field string) string {
	f.t.Helper()
	v := f.field(rec, t, field)
	if v.Kind == reasonr.KindNone {
		return "none"
	}
	return f.a.Text(v)
}

// findIDs looks up the rows of type t by the personal field field of the
// index index: one value — ==, several — in.
func (f *fixture) findIDs(t, index, field string, vals ...string) []reasonr.Value {
	f.t.Helper()
	ti := f.s.Type(f.typ(t))
	var ix reasonr.IndexRef = reasonr.NoIndex
	for i := range ti.Indexes {
		if ti.Indexes[i].Name == index {
			ix = ti.Indexes[i].Ref
		}
	}
	cond := reasonr.Cond{Field: reasonr.FieldPath{f.fieldRef(t, field)}, Op: reasonr.OpEq}
	if len(vals) == 1 {
		cond.Value = f.a.String(vals[0])
	} else {
		m := f.a.Mark()
		for _, v := range vals {
			f.a.Push(f.a.String(v))
		}
		cond.Op, cond.Value = reasonr.OpIn, f.a.End(reasonr.KindList, 0, m)
	}
	rows, err := f.st.Unit.Find(context.Background(), f.tenant, f.a, &reasonr.FindSpec{
		Type: ti.Ref, Index: ix, Where: []reasonr.Cond{cond}, Limit: 10, Cursor: reasonr.None,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	var ids []reasonr.Value
	for _, r := range f.a.Elems(rows.Records) {
		e := f.a.Elems(r)
		ids = append(ids, e[len(e)-1])
	}
	return ids
}

// cardID is the identifier of the card of the person p.
func (f *fixture) cardID(p reasonr.Value) reasonr.Value {
	return reasonr.Value{Kind: reasonr.KindID, Type: f.typ("PersonCard"), Lo: p.Lo, Hi: p.Hi, Ref: p.Ref}
}

// noPlaintext checks that the markers do not appear in the store's raw
// data, while the non-personal cityA does (the dump is not empty).
func (f *fixture) noPlaintext(markers ...string) {
	f.t.Helper()
	if f.st.Dump == nil {
		return
	}
	raw := f.st.Dump()
	if !bytes.Contains(raw, []byte(cityA)) {
		f.t.Fatalf("the store dump does not have even the non-personal value %q: the dump is incomplete", cityA)
	}
	for _, m := range markers {
		if bytes.Contains(raw, []byte(m)) {
			f.t.Errorf("plaintext %q in the store", m)
		}
	}
}

// outboxArgs returns the argument arg of the event event and the argument
// of the request Sms.send of the outbox units after the position from.
func (f *fixture) outboxArgs(from port.Position, event, arg string) (evs, reqs []string) {
	f.t.Helper()
	person := f.s.Type(f.typ("Person"))
	for _, u := range f.units(from) {
		for _, ee := range u.Entities {
			for _, ev := range ee.Events {
				mi := &person.Methods[ev.Name]
				if ee.Type != person.Ref || mi.Name != event {
					continue
				}
				for i, p := range mi.Params {
					if p.Name == arg {
						evs = append(evs, textOf(u.Arena, u.Arena.Elems(ev.Args.Value())[i]))
					}
				}
			}
		}
		for _, rq := range u.Requests {
			reqs = append(reqs, textOf(u.Arena, u.Arena.Elems(rq.Args.Value())[0]))
		}
	}
	return evs, reqs
}

func textOf(a *reasonr.Arena, v reasonr.Value) string {
	if v.Kind == reasonr.KindNone {
		return "none"
	}
	return a.Text(v)
}

// testPersonalStored: values are read but not stored as plaintext; search
// ==, in and uniqueness go by a blind index; the outbox is decrypted.
func testPersonalStored(t *testing.T, sf Factory) {
	f := newPersonal(t, sf)
	p1, p2, p3 := f.id("Person"), f.id("Person"), f.id("Person")
	if errs := f.register(p1, phoneA, nameA); len(errs) > 0 {
		t.Fatal(errs)
	}
	f.register(p2, phoneB, nameB)
	st, _, _ := f.load(p1, "Person")
	if f.text(st, "Person", "phone") != phoneA || f.text(st, "Person", "name") != nameA || f.text(st, "Person", "city") != cityA {
		t.Fatalf("load: %s", f.a.Format(st))
	}
	if card, _, _ := f.load(f.cardID(p1), "PersonCard"); f.text(card, "PersonCard", "title") != nameA {
		t.Fatalf("projection: %s", f.a.Format(card))
	}
	f.noPlaintext(phoneA, phoneB, nameA, nameB)

	if ids := f.findIDs("Person", "byPhone", "phone", phoneA); len(ids) != 1 || !reasonr.Equal(f.a, ids[0], f.a, p1) {
		t.Errorf("search ==: %d rows", len(ids))
	}
	if ids := f.findIDs("Person", "byPhone", "phone", phoneA, phoneB, "+70000000000"); len(ids) != 2 {
		t.Errorf("search in: %d rows", len(ids))
	}
	if ids := f.findIDs("Person", "byPhone", "phone", "+70000000000"); len(ids) != 0 {
		t.Errorf("a search for someone else's number: %d rows", len(ids))
	}
	if ids := f.findIDs("PersonCard", "byTitle", "title", nameB); len(ids) != 1 {
		t.Errorf("a search by projection: %d rows", len(ids))
	}
	if errs := f.register(p3, phoneA, "Victor"); len(errs) != 1 || !strings.HasPrefix(errs[0], "phoneTaken") {
		t.Errorf("uniqueness: %v", errs)
	}
	evs, reqs := f.outboxArgs(0, "registered", "newPhone")
	if len(evs) != 2 || evs[0] != phoneA || len(reqs) != 2 || reqs[0] != phoneA {
		t.Errorf("outbox: events %v, requests %v", evs, reqs)
	}
	// The subjects of personal event arguments — for the journal and the bus.
	u := f.units(0)[0]
	if ev := u.Entities[0].Events[0]; len(ev.Subjects) != 2 || !reasonr.Equal(u.Arena, ev.Subjects[0].Subject.ID, f.a, p1) {
		t.Errorf("event subjects in the outbox: %+v", ev.Subjects)
	}
}

// testPersonalApplyForget: the asynchronous projection store encrypts rows
// and erases the data of a forgotten subject only in the rows of the
// projections forget.Types.
func testPersonalApplyForget(t *testing.T, sf Factory) {
	f := newPersonal(t, sf)
	ps, ok := f.st.Unit.(kind.ProjectionStore)
	if !ok {
		t.Skip("the store does not keep asynchronous projections")
	}
	p1 := f.id("Person")
	f.register(p1, phoneA, nameA)
	ctx := context.Background()
	card := f.s.Type(f.typ("PersonCard"))
	other := f.cardID(f.id("Person"))
	op := reasonr.ProjectionOp{Kind: reasonr.ProjectionUpsert, Projection: card.Ref, ID: other, Set: []reasonr.FieldValue{
		{Field: f.fieldRef("PersonCard", "person"), Value: reasonr.Value{Kind: reasonr.KindID, Type: f.typ("Person"), Lo: p1.Lo, Hi: p1.Hi, Ref: p1.Ref}},
		{Field: f.fieldRef("PersonCard", "title"), Value: f.a.String(nameB)},
	}}
	keys := f.units(0)[0].Keys // the key version of p1 that decrypted the outbox
	if err := ps.Apply(ctx, f.tenant, 1, f.a, []reasonr.ProjectionOp{op}, keys, kind.Forget{}); err != nil {
		t.Fatal(err)
	}
	if c, _, _ := f.load(other, "PersonCard"); f.text(c, "PersonCard", "title") != nameB {
		t.Fatalf("projection: %s", f.a.Format(c))
	}
	f.noPlaintext(phoneA, nameA, nameB)
	forget := kind.Forget{Subjects: []reasonr.Subject{{Type: f.typ("Person"), ID: p1}}, Types: []reasonr.TypeRef{card.Ref}}
	if err := ps.Apply(ctx, f.tenant, 2, f.a, nil, nil, forget); err != nil {
		t.Fatal(err)
	}
	if c, _, _ := f.load(other, "PersonCard"); f.text(c, "PersonCard", "title") != "none" {
		t.Errorf("the projection after forget: %s", f.a.Format(c))
	}
	if ids := f.findIDs("PersonCard", "byTitle", "title", nameB); len(ids) != 0 {
		t.Errorf("the projection's blind value survived forget: %d rows", len(ids))
	}
	// Rows of other types are not touched: the main store destroys the key.
	if st, _, _ := f.load(p1, "Person"); f.text(st, "Person", "phone") != phoneA {
		t.Errorf("forget of asynchronous projections erased an entity: %s", f.a.Format(st))
	}
}

// testPersonalForget: forget erases a subject's data everywhere and frees
// the unique value; another subject's data is intact.
func testPersonalForget(t *testing.T, sf Factory) {
	f := newPersonal(t, sf)
	p1, p2, p3 := f.id("Person"), f.id("Person"), f.id("Person")
	f.register(p1, phoneA, nameA)
	f.register(p2, phoneB, nameB)
	_, v0, _ := f.load(p1, "Person")
	if errs := f.exec(support, p1, "Forget"); len(errs) > 0 {
		t.Fatal(errs)
	}
	st, v1, ok := f.load(p1, "Person")
	if !ok || f.text(st, "Person", "phone") != "none" || f.text(st, "Person", "name") != "none" || f.text(st, "Person", "city") != cityA {
		t.Fatalf("after forget: %s", f.a.Format(st))
	}
	if v1 <= v0 {
		t.Errorf("version after forget %d, was %d: a concurrent command will not get a conflict", v1, v0)
	}
	if card, _, _ := f.load(f.cardID(p1), "PersonCard"); f.text(card, "PersonCard", "title") != "none" {
		t.Errorf("the projection after forget: %s", f.a.Format(card))
	}
	if st, _, _ := f.load(p2, "Person"); f.text(st, "Person", "phone") != phoneB {
		t.Error("the data of another subject is erased")
	}
	if ids := f.findIDs("Person", "byPhone", "phone", phoneA); len(ids) != 0 {
		t.Errorf("the blind index of the forgotten one finds %d rows", len(ids))
	}
	if ids := f.findIDs("PersonCard", "byTitle", "title", nameA); len(ids) != 0 {
		t.Errorf("the projection blind index of the forgotten one finds %d rows", len(ids))
	}
	evs, reqs := f.outboxArgs(0, "registered", "newPhone")
	if len(evs) != 2 || evs[0] != "none" || evs[1] != phoneB || reqs[0] != "none" || reqs[1] != phoneB {
		t.Errorf("outbox after forget: events %v, requests %v", evs, reqs)
	}
	if errs := f.register(p3, phoneA, "Victor"); len(errs) > 0 {
		t.Errorf("the number of the forgotten one is not freed: %v", errs)
	}
	f.noPlaintext(phoneB, nameA, nameB)
}

// testPersonalForgetFailed: forget in a command with an error deletes nothing.
func testPersonalForgetFailed(t *testing.T, sf Factory) {
	f := newPersonal(t, sf)
	p1 := f.id("Person")
	f.register(p1, phoneA, nameA)
	if errs := f.exec(support, p1, "ForgetFailed"); len(errs) != 1 {
		t.Fatalf("errors: %v", errs)
	}
	if st, _, _ := f.load(p1, "Person"); f.text(st, "Person", "phone") != phoneA {
		t.Errorf("a rejected command erased data: %s", f.a.Format(st))
	}
	if evs, _ := f.outboxArgs(0, "registered", "newPhone"); len(evs) != 1 || evs[0] != phoneA {
		t.Errorf("a rejected command destroyed the key: %v", evs)
	}
}

// testPersonalRebirth: after forget a subject gets data again under a new
// key; the old data stays unreadable.
func testPersonalRebirth(t *testing.T, sf Factory) {
	f := newPersonal(t, sf)
	p1 := f.id("Person")
	f.register(p1, phoneA, nameA)
	f.exec(support, p1, "Forget")
	if errs := f.register(p1, phoneB, nameB); len(errs) > 0 {
		t.Fatal(errs)
	}
	if st, _, _ := f.load(p1, "Person"); f.text(st, "Person", "phone") != phoneB || f.text(st, "Person", "name") != nameB {
		t.Fatalf("after signing up again: %s", f.a.Format(st))
	}
	if ids := f.findIDs("Person", "byPhone", "phone", phoneB); len(ids) != 1 {
		t.Errorf("the new number is not found: %d rows", len(ids))
	}
	if evs, _ := f.outboxArgs(0, "registered", "newPhone"); len(evs) != 2 || evs[0] != "none" || evs[1] != phoneB {
		t.Errorf("outbox: %v", evs)
	}
}

// testPersonalTenants: blind indexes and keys are per tenant.
func testPersonalTenants(t *testing.T, sf Factory) {
	f := newPersonal(t, sf)
	p1 := f.id("Person")
	f.register(p1, phoneA, nameA)
	f.tenant = T2
	p2 := f.id("Person")
	if errs := f.register(p2, phoneA, nameA); len(errs) > 0 {
		t.Fatalf("the number is taken in another tenant: %v", errs)
	}
	f.exec(support, p2, "Forget")
	f.tenant = T1
	if st, _, _ := f.load(p1, "Person"); f.text(st, "Person", "phone") != phoneA {
		t.Errorf("forget in another tenant erased data: %s", f.a.Format(st))
	}
}

// testPersonalRace: forget of a subject between running a command and its
// commit. The write must leave neither data under the old key nor a blind
// value of the forgotten number.
func testPersonalRace(t *testing.T, sf Factory) {
	f := newPersonal(t, sf)
	p1 := f.id("Person")
	f.register(p1, phoneA, nameA)
	fired := false
	f.hook = func(w *reasonr.UnitOfWork) {
		if fired || len(w.Entities) == 0 || len(w.Forget) > 0 {
			return
		}
		fired = true
		f.hook = nil
		if errs := f.exec(support, p1, "Forget"); len(errs) > 0 {
			t.Errorf("forget: %v", errs)
		}
	}
	if errs := f.exec(actor(p1), p1, "Rename", f.a.String(nameB)); len(errs) > 0 {
		t.Fatal(errs)
	}
	if !fired {
		t.Fatal("forget did not run")
	}
	st, _, _ := f.load(p1, "Person")
	if name := f.text(st, "Person", "name"); name != nameB && name != "none" {
		t.Errorf("the name after the race: %s", name)
	}
	if f.text(st, "Person", "phone") != "none" {
		t.Errorf("the phone survived forget: %s", f.a.Format(st))
	}
	if ids := f.findIDs("Person", "byPhone", "phone", phoneA); len(ids) != 0 {
		t.Errorf("the blind value survived forget: %d rows", len(ids))
	}
}

// testPersonalStaleKey: another engine instance that has the key of a
// forgotten subject in its cache writes the subject's data. The commit must
// notice that the key is destroyed (the key check, RFC §20), and the
// command repeats under a new key; otherwise the data would be written with
// a destroyed key and lost.
func testPersonalStaleKey(t *testing.T, sf Factory) {
	f := newPersonal(t, sf)
	if f.st.Twin == nil {
		t.Skip("the store has no second instance")
	}
	twin, err := kernel.New(kernel.Config{Programs: f.p, Store: f.st.Twin, Clock: f.clock})
	if err != nil {
		t.Fatal(err)
	}
	rename := func(k *kernel.Kernel, p reasonr.Value, name string) {
		t.Helper()
		res, err := k.Command(context.Background(), &kind.CommandRequest{Tenant: f.tenant, Call: reasonr.CommandCall{
			Entity: p.Type, ID: p, Command: f.method(p.Type, "Rename"), Args: f.a.Record(0, f.a.String(name)), Arena: f.a, Caller: actor(p),
		}})
		if err != nil {
			t.Fatal(err)
		}
		defer res.Release()
		if len(res.Errors) > 0 {
			t.Fatal(errorTexts(res))
		}
	}
	p1 := f.id("Person")
	f.register(p1, phoneA, nameA)
	rename(twin, p1, "Intermediate") // the subject key is in the second instance's cache
	if errs := f.exec(support, p1, "Forget"); len(errs) > 0 {
		t.Fatal(errs)
	}
	rename(twin, p1, nameB)
	if st, _, _ := f.load(p1, "Person"); f.text(st, "Person", "name") != nameB {
		t.Errorf("a name written with a stale key: %s", f.a.Format(st))
	}
}
