package conformance

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/codecs"
	"github.com/tycheg/reasonr-engine/internal/dispatch"
	"github.com/tycheg/reasonr-engine/internal/pii"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/internal/rows"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Refusals after forget (RFC §20): a subject is forgotten while its data
// still waits in a timer, in an external request in the outbox or in a
// response by idempotency key.

// testPersonalTimers: timer arguments are stored encrypted; after forget a
// command with a non-optional personal argument does not run and the timer
// is deleted, and one with an optional argument gets none.
func testPersonalTimers(t *testing.T, sf Factory) {
	f := newPersonal(t, sf)
	if f.st.Timers == nil {
		t.Skip("the store does not keep timers")
	}
	p1, p2 := f.id("Person"), f.id("Person")
	f.register(p1, phoneA, nameA)
	f.register(p2, phoneB, nameB)
	for _, x := range []struct {
		p     reasonr.Value
		phone string
	}{{p1, phoneA}, {p2, phoneB}} {
		if errs := f.exec(actor(x.p), x.p, "Remind", f.a.String(x.phone)); len(errs) > 0 {
			t.Fatal(errs)
		}
	}
	f.noPlaintext(phoneA, phoneB)
	if errs := f.exec(support, p1, "Forget"); len(errs) > 0 {
		t.Fatal(errs)
	}
	before := lastPosition(f)
	fired := f.advance(Start.Add(2 * time.Hour))
	if len(fired) != 3 {
		t.Errorf("%d commands fired, want 3: the command of the forgotten one with a non-optional argument ran", len(fired))
	}
	if strict, _ := f.outboxArgs(before, "reminded", "text"); !slices.Equal(strict, []string{phoneB}) {
		t.Errorf("reminded: %v", strict)
	}
	if soft, _ := f.outboxArgs(before, "remindedSoft", "text"); !slices.Equal(soft, []string{"none", phoneB}) {
		t.Errorf("remindedSoft: %v", soft)
	}
	if ts := f.lease(T1, Start.Add(3*time.Hour), time.Second); len(ts) != 0 {
		t.Errorf("the timer of the forgotten one is left: %d", len(ts))
	}
}

// testPersonalReplay: a response by idempotency key keeps the personal
// arguments of notifications and errors encrypted; a command repeated after
// forget gets none in them.
func testPersonalReplay(t *testing.T, sf Factory) {
	f := newPersonal(t, sf)
	p1 := f.id("Person")
	f.register(p1, phoneA, nameA)
	call := func(key string, fail bool) (notes, errs []string, replayed bool) {
		t.Helper()
		res := f.command(key, actor(p1), p1, "Call", f.a.String(phoneA), reasonr.Bool(fail))
		defer res.Release()
		for _, n := range res.Notifications {
			notes = append(notes, textOf(res.Arena, res.Arena.Elems(n.Args.Value())[0]))
		}
		for _, be := range res.Errors {
			errs = append(errs, textOf(res.Arena, res.Arena.Elems(be.Args.Value())[0]))
		}
		return notes, errs, res.Replayed
	}
	if notes, _, _ := call("call-ok", false); !slices.Equal(notes, []string{phoneA}) {
		t.Fatalf("notifications: %v", notes)
	}
	if _, errs, _ := call("call-fail", true); !slices.Equal(errs, []string{phoneA}) {
		t.Fatalf("errors: %v", errs)
	}
	f.noPlaintext(phoneA)
	if notes, _, replayed := call("call-ok", false); !replayed || !slices.Equal(notes, []string{phoneA}) {
		t.Errorf("a repeat before forget: %v, replayed %v", notes, replayed)
	}
	if errs := f.exec(support, p1, "Forget"); len(errs) > 0 {
		t.Fatal(errs)
	}
	if notes, _, replayed := call("call-ok", false); !replayed || !slices.Equal(notes, []string{"none"}) {
		t.Errorf("a repeated notification after forget: %v, replayed %v", notes, replayed)
	}
	if _, errs, replayed := call("call-fail", true); !replayed || !slices.Equal(errs, []string{"none"}) {
		t.Errorf("a repeated error after forget: %v, replayed %v", errs, replayed)
	}
}

// sent is a transport that remembers the to argument of sent requests.
type sent struct{ to []string }

func (s *sent) Send(_ context.Context, r *kind.LeasedRequest) error {
	s.to = append(s.to, textOf(r.Arena, r.Arena.Elems(r.Request.Args.Value())[0]))
	return nil
}

// testPersonalRequests: external requests waiting to be sent keep personal
// arguments as ciphertext; a request whose non-optional personal argument's
// subject was forgotten after commit is not sent (RFC §20.3); the others go
// decrypted.
func testPersonalRequests(t *testing.T, sf Factory) {
	f := newPersonal(t, sf)
	q, ok := f.st.Unit.(kind.RequestQueue)
	if !ok {
		t.Skip("the main store does not keep external requests")
	}
	const phoneC = "+79990000003"
	p1, p2, p3 := f.id("Person"), f.id("Person"), f.id("Person")
	f.register(p1, phoneA, nameA)
	f.register(p2, phoneB, nameB)
	f.register(p3, phoneC, "Vera")
	f.noPlaintext(phoneA, phoneB, phoneC)
	if errs := f.exec(support, p1, "Forget"); len(errs) > 0 {
		t.Fatal(errs)
	}
	if errs := f.exec(support, p3, "Forget"); len(errs) > 0 {
		t.Fatal(errs)
	}
	tr := &sent{}
	s := &dispatch.Sender{Queue: q, Default: tr, Catalog: f.p, Clock: f.clock, Workers: 1}
	if n, err := s.SendDue(context.Background()); err != nil || n != 3 {
		t.Fatalf("%d attempts: %v", n, err)
	}
	if !slices.Equal(tr.to, []string{phoneB}) {
		t.Errorf("requests sent with to = %v, want only %s", tr.to, phoneB)
	}
	if rs, _ := q.LeaseRequests(context.Background(), Start.Add(time.Hour), 10, time.Minute); len(rs) != 0 {
		t.Errorf("%d requests left", len(rs))
	}
}

// testPersonalForgetLog: the forget journal records the subjects a unit of
// work forgot; a redelivery duplicates nothing. Skipped if the store does
// not keep the journal.
func testPersonalForgetLog(t *testing.T, sf Factory) {
	f := newPersonal(t, sf)
	log, ok := rows.As[port.ForgetLog](f.st.Unit)
	if !ok {
		t.Skip("the store does not keep the forget journal")
	}
	p1, p2 := f.id("Person"), f.id("Person")
	f.register(p1, phoneA, nameA)
	f.register(p2, phoneB, nameB)
	f.clock.set(Start.Add(time.Hour))
	f.exec(support, p1, "Forget")
	f.exec(support, p1, "ForgetFailed") // an error: not journaled
	c := &dispatch.Forgets{Log: log, Catalog: f.p}
	for range 2 {
		for _, u := range f.units(0) {
			if err := c.Deliver(context.Background(), &u, -1, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	id, err := codecs.JSONOf(f.s, codecs.Options{}).Append(nil, f.a, reasonr.Type{Kind: reasonr.KindID, Ref: p1.Type}, p1)
	if err != nil {
		t.Fatal(err)
	}
	ref := port.SubjectRef{Type: "Person", ID: string(id)}
	got, err := log.Forgotten(context.Background(), Start)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Tenant != T1 || got[0].Subject != ref || !got[0].At.Equal(Start.Add(time.Hour)) || got[0].Position == 0 {
		t.Fatalf("journal: %+v, want %+v", got, ref)
	}
	if later, err := log.Forgotten(context.Background(), Start.Add(2*time.Hour)); err != nil || len(later) != 0 {
		t.Errorf("entries after the moment: %+v, %v", later, err)
	}
}

// testPersonalJournal: the event store keeps personal arguments encrypted
// with the keys of the subjects from Event.Subjects. Skipped if the store
// cannot be an event store.
func testPersonalJournal(t *testing.T, sf Factory) {
	f := newPersonal(t, sf)
	sink, ok := f.st.Unit.(ports.EventStore)
	if !ok {
		t.Skip("the store cannot be an event store")
	}
	p1 := f.id("Person")
	f.register(p1, phoneA, nameA)
	c := &dispatch.Events{Sink: sink}
	for _, u := range f.units(0) {
		if err := c.Deliver(context.Background(), &u, -1, nil); err != nil {
			t.Fatal(err)
		}
	}
	f.noPlaintext(phoneA, nameA)
}

// testPersonalBus: the bus gets events from the event store without
// personal arguments — null (RFC §20.3), non-personal ones as is. Skipped
// if the store does not give events to the bus.
func testPersonalBus(t *testing.T, sf Factory) {
	f := newPersonal(t, sf)
	sink, ok := f.st.Unit.(ports.EventStore)
	feed, ok2 := rows.As[port.EventFeed](f.st.Unit)
	if !ok || !ok2 {
		t.Skip("the store cannot be an event store for the bus")
	}
	p1 := f.id("Person")
	f.register(p1, phoneA, nameA)
	c := &dispatch.Events{Sink: sink}
	units := f.units(0)
	for _, u := range units {
		if err := c.Deliver(context.Background(), &u, -1, nil); err != nil {
			t.Fatal(err)
		}
	}
	msgs, err := feed.ReadEvents(context.Background(), 0, units[len(units)-1].Position, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Name != "registered" {
		t.Fatalf("%d messages: %+v", len(msgs), msgs)
	}
	var got map[string]any
	if err := json.Unmarshal(msgs[0].Args, &got); err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"newPhone": nil, "newName": nil, "newCity": cityA}; !reflect.DeepEqual(got, want) {
		t.Errorf("arguments in the bus: %s", msgs[0].Args)
	}
}

// fanout is a hub that only broadcasts forgotten subjects to subscribers.
type fanout struct {
	kind.NotificationHub
	fns []func(port.Tenant, []port.SubjectRef)
}

func (h *fanout) Publish(context.Context, port.Tenant, *reasonr.Arena, []reasonr.Notification) error {
	return nil
}

func (h *fanout) PublishForgotten(_ context.Context, tenant port.Tenant, subjects []port.SubjectRef) error {
	for _, fn := range h.fns {
		fn(tenant, subjects)
	}
	return nil
}

func (h *fanout) OnForgotten(fn func(port.Tenant, []port.SubjectRef)) { h.fns = append(h.fns, fn) }

// testPersonalBroadcast: after forget the kernel broadcasts the forgotten
// subjects through the hub, and another instance drops their keys from its
// cache: otherwise it would decrypt the outbox with the old key until the
// cache expires.
func testPersonalBroadcast(t *testing.T, sf Factory) {
	hub := &fanout{}
	f := newFixture(t, sf, options{machine: personal.get(t), hub: hub})
	twin, ok := f.st.Twin.(kind.EffectSource)
	cache, cok := rows.As[ports.KeyCache](f.st.Twin)
	if !ok || !cok {
		t.Skip("the store has no second instance with an outbox and a key cache")
	}
	hub.OnForgotten(cache.DropKeys)
	p1 := f.id("Person")
	f.register(p1, phoneA, nameA)
	read := func() string {
		t.Helper()
		us, err := twin.Read(context.Background(), 0, 1)
		if err != nil || len(us) != 1 {
			t.Fatalf("the outbox of the second instance: %d, %v", len(us), err)
		}
		return textOf(us[0].Arena, us[0].Arena.Elems(us[0].Requests[0].Args.Value())[0])
	}
	if got := read(); got != phoneA { // the key is in the second instance's cache
		t.Fatalf("before forget: %s", got)
	}
	if errs := f.exec(support, p1, "Forget"); len(errs) > 0 {
		t.Fatal(errs)
	}
	if got := read(); got != "none" {
		t.Errorf("the second instance decrypted the outbox with the key of the forgotten one: %s", got)
	}
}

// testPersonalReseal: asynchronous projections and the event store encrypt
// values with the key versions that decrypted them from the outbox
// (CommittedUnit.Keys, RFC §31.2) and create no keys. If the subject is
// forgotten after the outbox is read — and even got a key of a new version
// again — the value is written as none, without a blind value.
func testPersonalReseal(t *testing.T, sf Factory) {
	f := newPersonal(t, sf)
	ps, ok := f.st.Unit.(kind.ProjectionStore)
	if !ok {
		t.Skip("the store does not keep asynchronous projections")
	}
	p1 := f.id("Person")
	f.register(p1, phoneA, nameA)
	u := f.units(0)[0]
	if !slices.ContainsFunc(u.Keys, func(kv port.KeyVersion) bool { return kv.Subject.Type == "Person" && kv.Version > 0 }) {
		t.Fatalf("the key versions of the unit from the outbox: %+v", u.Keys)
	}
	ctx := context.Background()
	pos := port.Position(0)
	apply := func(keys []port.KeyVersion) string {
		t.Helper()
		card := f.cardID(f.id("Person"))
		op := reasonr.ProjectionOp{Kind: reasonr.ProjectionUpsert, Projection: f.typ("PersonCard"), ID: card, Set: []reasonr.FieldValue{
			{Field: f.fieldRef("PersonCard", "person"), Value: reasonr.Value{Kind: reasonr.KindID, Type: f.typ("Person"), Lo: p1.Lo, Hi: p1.Hi, Ref: p1.Ref}},
			{Field: f.fieldRef("PersonCard", "title"), Value: f.a.String(nameA)},
		}}
		pos++
		if err := ps.Apply(ctx, f.tenant, pos, f.a, []reasonr.ProjectionOp{op}, keys, kind.Forget{}); err != nil {
			t.Fatal(err)
		}
		c, _, _ := f.load(card, "PersonCard")
		return f.text(c, "PersonCard", "title")
	}
	if got := apply(u.Keys); got != nameA {
		t.Fatalf("before forget: %s", got)
	}
	if errs := f.exec(support, p1, "Forget"); len(errs) > 0 {
		t.Fatal(errs)
	}
	if got := apply(u.Keys); got != "none" {
		t.Errorf("after forget the value is written with a new key: %s", got)
	}
	if errs := f.register(p1, phoneB, nameB); len(errs) > 0 {
		t.Fatal(errs)
	}
	if got := apply(u.Keys); got != "none" {
		t.Errorf("after signing up again the value is written with its key: %s", got)
	}
	if got := apply(nil); got != "none" {
		t.Errorf("a value without a key version is written: %s", got)
	}
	if ids := f.findIDs("PersonCard", "byTitle", "title", nameA); len(ids) != 0 {
		t.Errorf("the blind value of the forgotten one is written: %d rows", len(ids))
	}
	if sink, ok := f.st.Unit.(ports.EventStore); ok {
		if err := sink.Append(ctx, []kind.CommittedUnit{u}); err != nil {
			t.Fatal(err)
		}
	}
	f.noPlaintext(phoneA, nameA, phoneB, nameB)
}

// testPersonalSeparate: asynchronous projections and the event store in a
// separate database encrypt with the main store's keys (RFC §31.2), so
// forget in the main store makes their data unreadable too. The separate
// store's transaction must not get into reading the main store's keys.
func testPersonalSeparate(t *testing.T, sf Factory) {
	f := newPersonal(t, sf)
	ps, ok := f.st.Separate.(kind.ProjectionStore)
	sep, sok := f.st.Separate.(pii.Shared)
	main, mok := f.st.Unit.(pii.Shared)
	if !ok || !sok || !mok {
		t.Skip("no separate projection store with shared keys")
	}
	ring := main.Keyring()
	sep.UseKeyring(ring)
	p1 := f.id("Person")
	f.register(p1, phoneA, nameA)
	u := f.units(0)[0]
	ctx := context.Background()
	// An empty cache: the keys are read from the main store inside the
	// transaction of the separate one.
	for _, kv := range u.Keys {
		ring.Forgotten(f.tenant, []port.SubjectRef{kv.Subject})
	}
	ring.DropTenant(f.tenant)
	card := f.cardID(p1)
	op := reasonr.ProjectionOp{Kind: reasonr.ProjectionUpsert, Projection: f.typ("PersonCard"), ID: card, Set: []reasonr.FieldValue{
		{Field: f.fieldRef("PersonCard", "person"), Value: reasonr.Value{Kind: reasonr.KindID, Type: f.typ("Person"), Lo: p1.Lo, Hi: p1.Hi, Ref: p1.Ref}},
		{Field: f.fieldRef("PersonCard", "title"), Value: f.a.String(nameA)},
	}}
	if err := ps.Apply(ctx, f.tenant, 1, f.a, []reasonr.ProjectionOp{op}, u.Keys, kind.Forget{}); err != nil {
		t.Fatal(err)
	}
	if sink, ok := f.st.Separate.(ports.EventStore); ok {
		if err := sink.Append(ctx, []kind.CommittedUnit{u}); err != nil {
			t.Fatal(err)
		}
	}
	title := func() string {
		t.Helper()
		rec, _, ok, err := ps.Load(ctx, f.tenant, f.a, f.typ("PersonCard"), card)
		if err != nil || !ok {
			t.Fatalf("a projection in the separate database: %v, %v", ok, err)
		}
		return f.text(rec.Value(), "PersonCard", "title")
	}
	if got := title(); got != nameA {
		t.Fatalf("before forget: %s", got)
	}
	if errs := f.exec(support, p1, "Forget"); len(errs) > 0 {
		t.Fatal(errs)
	}
	if got := title(); got != "none" {
		t.Errorf("after forget in the main store the separate database is readable: %s", got)
	}
}

// testPersonalRotate: master key rotation (reasonrd pii rotate) rewraps
// the tenant keys in the store: data reads with keys unwrapped by the new
// master key and does not read with the previous one; a repeated rotation
// changes nothing.
func testPersonalRotate(t *testing.T, sf Factory) {
	f := newPersonal(t, sf)
	rot, ok := rows.As[port.TenantKeyRotator](f.st.Unit)
	sh, sok := f.st.Unit.(pii.Shared)
	if !ok || !sok || f.st.KMS == nil {
		t.Skip("the store does not rewrap tenant keys")
	}
	p1 := f.id("Person")
	f.register(p1, phoneA, nameA)
	ctx := context.Background()
	next := pii.NewMemoryKMS()
	res, err := pii.Rotate(ctx, rot, f.st.KMS, next)
	if err != nil || res.Rotated != 1 || res.Skipped != 0 {
		t.Fatalf("rotation: %+v, %v", res, err)
	}
	ring := sh.Keyring()
	sh.UseKeyring(ring.WithKMS(next))
	if st, _, _ := f.load(p1, "Person"); f.text(st, "Person", "phone") != phoneA {
		t.Errorf("after rotation: %s", f.a.Format(st))
	}
	if res, err := pii.Rotate(ctx, rot, f.st.KMS, next); err != nil || res.Rotated != 0 || res.Skipped != 1 {
		t.Errorf("a repeated rotation: %+v, %v", res, err)
	}
	sh.UseKeyring(ring.WithKMS(f.st.KMS))
	if _, _, _, err := f.st.Unit.Load(ctx, f.tenant, f.a, f.typ("Person"), p1); err == nil {
		t.Error("the previous master key unwraps the tenant key after rotation")
	}
}
