package testapi

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/memory"
	"github.com/tycheg/reasonr-engine/internal/codecs"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/internal/pii"
	"github.com/tycheg/reasonr-engine/internal/testmodels"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// TestFakes checks the fake transport and the fake call consumer (RFC
// §18.1): a repeated request delivery does not run the fake twice, a
// repeated unit with calls does not run the model commands twice; requests
// and calls of an ephemeral tenant are skipped — the cascade runs them.
func TestFakes(t *testing.T) {
	mach, err := reasonr.Build(testmodels.Dir("taxi"), reasonr.Options{Test: true})
	if err != nil {
		t.Fatal(err)
	}
	s := mach.Schema()
	const eph = port.Tenant("test-1")
	programs := kernel.NewPrograms()
	programs.Set(port.DefaultTenant, mach)
	programs.Set(eph, mach)
	store := memory.NewStoreKMS(programs, pii.NewMemoryKMS())
	k, err := kernel.New(kernel.Config{Programs: programs, Store: store, Timers: store})
	if err != nil {
		t.Fatal(err)
	}
	api := &API{Kernel: k, Programs: programs, Store: store, Effects: store, Clock: NewClock(),
		tenants: map[port.Tenant]*ephemeral{eph: {a: reasonr.NewArena()}}}
	var log bytes.Buffer
	f := api.Fakes(slog.New(slog.NewTextHandler(&log, nil)))
	ctx := context.Background()

	id := func(typ string, n byte) reasonr.Value {
		return reasonr.ID(s.Lookup(typ).Ref, reasonr.UUID([16]byte{15: n}))
	}
	c1, d1, o1 := id("Customer", 1), id("Driver", 1), id("Order", 1)
	exec := func(tenant port.Tenant, caller reasonr.Caller, target reasonr.Value, command, args string) {
		t.Helper()
		ti := s.Type(target.Type)
		mi := ti.Method(command)
		a := reasonr.NewArena()
		rec, err := codecs.JSONOf(s, codecs.Options{}).DecodeFields(a, 0, mi.Params, []byte(args))
		if err != nil {
			t.Fatalf("%s: %v", command, err)
		}
		res, err := k.Command(ctx, &kind.CommandRequest{Tenant: tenant, Call: reasonr.CommandCall{
			Entity: target.Type, ID: target, Command: mi.Ref, Args: rec, Arena: a, Caller: caller}})
		if err != nil || len(res.Errors) > 0 {
			t.Fatalf("%s: %v %v", command, err, res.Errors)
		}
		res.Release()
	}
	actor := func(v reasonr.Value) reasonr.Caller {
		return reasonr.Caller{Kind: reasonr.CallerUser, Subject: v, Actor: v.Type}
	}
	units := func() []kind.CommittedUnit {
		t.Helper()
		us, err := store.Read(ctx, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		return us
	}
	matching := reasonr.FakeID(s.Lookup("Matching").Ref)
	for _, tn := range []port.Tenant{port.DefaultTenant, eph} {
		exec(tn, reasonr.Caller{Kind: reasonr.CallerTest, Subject: reasonr.None}, matching, "Setup", `{"freeDrivers":["`+uuidOf(d1)+`"]}`)
		exec(tn, reasonr.Caller{Kind: reasonr.CallerUser, Subject: reasonr.None, Roles: []string{s.Role("admin")}}, d1, "Register",
			`{"driverName":"John","driverCar":{"plate":"A001AA77","model":"Kia Rio","carClass":"economy"}}`)
		exec(tn, actor(d1), d1, "GoOnline", `{}`)
		exec(tn, actor(c1), c1, "SignUp", `{"phoneNumber":"+79990000000"}`)
		exec(tn, actor(c1), c1, "OrderTaxi", `{"orderId":"`+uuidOf(o1)+
			`","orderRoute":{"from":{"lat":55.75,"lon":37.62},"to":{"lat":55.75,"lon":37.62},"distanceKm":3},"requestedClass":"economy"}`)
	}
	rs, err := store.LeaseRequests(ctx, time.Now(), 10, time.Minute)
	if err != nil || len(rs) != 2 {
		t.Fatalf("requests: %d, %v", len(rs), err)
	}
	before := len(units())
	for i := range rs {
		for range 2 { // a repeated delivery
			if err := f.Send(ctx, &rs[i]); err != nil {
				t.Fatal(err)
			}
		}
	}
	us := units()
	if len(us) != before+1 || us[before].Tenant != port.DefaultTenant || len(us[before].Calls) != 1 {
		t.Fatalf("%d units after the requests (was %d): the fake ran more than once or ran in an ephemeral tenant", len(us), before)
	}
	// A fake call of an ephemeral tenant: a unit with the same call.
	exec(eph, reasonr.Caller{Kind: reasonr.CallerFake, Subject: reasonr.None}, matching, "findDriver", requestArgs(t, s, &rs, eph))
	us = units()
	for i := before; i < len(us); i++ {
		for range 2 { // a repeated unit
			if err := f.Deliver(ctx, &us[i], -1, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got := units(); len(got) != len(us)+1 || got[len(us)].Tenant != port.DefaultTenant {
		t.Fatalf("%d units after the fake calls (was %d): a model command ran more than once or in an ephemeral tenant", len(got), len(us))
	}
	if strings.Contains(log.String(), "level=WARN") {
		t.Fatalf("log:\n%s", log.String())
	}

	// A request the fake has no implementation for is only logged.
	find := &s.Lookup("Matching").Methods[s.Lookup("Matching").Method("findDriver").Ref]
	find.Fake = false
	defer func() { find.Fake = true }()
	c2 := id("Customer", 2)
	exec(port.DefaultTenant, actor(c2), c2, "SignUp", `{"phoneNumber":"+79990000001"}`)
	exec(port.DefaultTenant, actor(c2), c2, "OrderTaxi", `{"orderId":"`+uuidOf(id("Order", 2))+
		`","orderRoute":{"from":{"lat":55.75,"lon":37.62},"to":{"lat":55.75,"lon":37.62},"distanceKm":3},"requestedClass":"economy"}`)
	if rs, err = store.LeaseRequests(ctx, time.Now(), 10, time.Minute); err != nil || len(rs) != 1 {
		t.Fatalf("requests: %d, %v", len(rs), err)
	}
	before = len(units())
	if err := f.Send(ctx, &rs[0]); err != nil || len(units()) != before || !strings.Contains(log.String(), "no implementation of the request") {
		t.Fatalf("a request without an implementation: %v, %d units (was %d)\n%s", err, len(units()), before, log.String())
	}
}

// requestArgs gives the request arguments of the tenant tenant from the leased rs in JSON.
func requestArgs(t *testing.T, s *reasonr.Schema, rs *[]kind.LeasedRequest, tenant port.Tenant) string {
	t.Helper()
	for _, r := range *rs {
		if r.Tenant == tenant {
			ti := s.Type(r.Request.System)
			b, err := codecs.JSONOf(s, codecs.Options{}).AppendFields(nil, r.Arena, ti.Methods[r.Request.Request].Params, r.Request.Args)
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
	}
	t.Fatalf("no request of tenant %s", tenant)
	return ""
}

// uuidOf is the uuid of an identifier as a string.
func uuidOf(id reasonr.Value) string {
	b, _ := codecs.JSONOf(&reasonr.Schema{}, codecs.Options{}).Append(nil, nil, reasonr.Type{Kind: reasonr.KindUUID}, reasonr.UUID(id.UUID()))
	return string(b[1 : len(b)-1])
}
