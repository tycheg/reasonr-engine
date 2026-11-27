package features_test

import (
	"context"
	"testing"
	"time"

	"github.com/tycheg/reasonr-engine/internal/codecs"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/memory"
	"github.com/tycheg/reasonr-engine/internal/features"
	"github.com/tycheg/reasonr-engine/port"
)

const customer = "0192b6c1-0000-4000-8000-000000000007"

func schema() *reasonr.Schema {
	return &reasonr.Schema{
		Types: []reasonr.TypeInfo{{Ref: 1, Name: "Sales.Customer", Kind: reasonr.TypeActor, ID: reasonr.IDUUID}},
		Roles: []string{"admin", "support"},
		Features: []reasonr.FeatureInfo{
			{Name: "Sales.actor", Date: "2026-11-07"}, {Name: "Sales.role", Date: "2026-11-07"},
			{Name: "Sales.all", Date: "2026-11-07"}, {Name: "Sales.tenant", Date: "2026-11-07"},
		},
	}
}

func on(t *testing.T, r *features.Resolver, feature string, sc port.FeatureScope) {
	t.Helper()
	if err := r.Change(context.Background(), port.FeatureChange{Feature: feature, On: true, Scope: sc, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func names(s *reasonr.Schema, f reasonr.Features) []string {
	var out []string
	for i := range s.Features {
		if f.Has(i) {
			out = append(out, s.Features[i].Name)
		}
	}
	return out
}

func TestLevels(t *testing.T) {
	s := schema()
	hub := memory.NewHub(false)
	r := features.New(memory.NewStore(nil), hub, nil)
	other := features.New(r.Store, hub, nil) // another instance: learns through the hub
	on(t, r, "Sales.actor", port.FeatureScope{Actor: "Sales.Customer:" + customer, Tenant: "acme"})
	on(t, r, "Sales.role", port.FeatureScope{Role: "admin", Tenant: "acme"})
	on(t, r, "Sales.tenant", port.FeatureScope{Tenant: "acme"})
	on(t, r, "Sales.removed", port.FeatureScope{}) // the flag is not in the code
	a := reasonr.NewArena()
	id, err := codecs.JSONOf(s, codecs.Options{}).DecodeIDText(a, 1, customer)
	if err != nil {
		t.Fatal(err)
	}
	actor := reasonr.Caller{Kind: reasonr.CallerUser, Actor: 1, Subject: id}
	admin := reasonr.Caller{Kind: reasonr.CallerUser, Roles: []string{"admin"}, Subject: reasonr.None}
	timer := reasonr.Caller{Kind: reasonr.CallerTimer, Subject: reasonr.None}
	for _, c := range []struct {
		name   string
		tenant port.Tenant
		caller reasonr.Caller
		want   string
	}{
		{"actor", "acme", actor, "[Sales.actor Sales.tenant]"},
		{"actor in another tenant", "beta", actor, "[]"},
		{"role", "acme", admin, "[Sales.role Sales.tenant]"},
		{"role in another tenant", "beta", admin, "[]"},
		{"timer", "acme", timer, "[Sales.tenant]"},
	} {
		for _, res := range []*features.Resolver{r, other} {
			if got := fmtNames(names(s, res.Features(c.tenant, s, &c.caller, a))); got != c.want {
				t.Errorf("%s: %s, want %s", c.name, got, c.want)
			}
		}
	}
	on(t, r, "Sales.all", port.FeatureScope{})
	if got := fmtNames(names(s, other.Features("beta", s, &timer, a))); got != "[Sales.all]" {
		t.Errorf("the whole system: %s", got)
	}
	if err := r.Change(context.Background(), port.FeatureChange{Feature: "Sales.tenant", Everywhere: true, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if got := fmtNames(names(s, other.Features("acme", s, &timer, a))); got != "[Sales.all]" {
		t.Errorf("after off: %s", got)
	}
	r.SetTest("test-1", "Sales.role")
	if got := fmtNames(names(s, r.Features("test-1", s, &timer, a))); got != "[Sales.role Sales.all]" {
		t.Errorf("a test flag: %s", got)
	}
	r.DropTest("test-1")
	if got := fmtNames(names(s, r.Features("test-1", s, &timer, a))); got != "[Sales.all]" {
		t.Errorf("after the test: %s", got)
	}
}

func fmtNames(ns []string) string {
	out := "["
	for i, n := range ns {
		if i > 0 {
			out += " "
		}
		out += n
	}
	return out + "]"
}

// TestAllocs: the caller's flag set is computed without allocating memory
// (RFC §23.4).
func TestAllocs(t *testing.T) {
	s := schema()
	r := features.New(memory.NewStore(nil), nil, nil)
	on(t, r, "Sales.actor", port.FeatureScope{Actor: "Sales.Customer:" + customer})
	on(t, r, "Sales.role", port.FeatureScope{Role: "admin", Tenant: "acme"})
	a := reasonr.NewArena()
	id, _ := codecs.JSONOf(s, codecs.Options{}).DecodeIDText(a, 1, customer)
	c := reasonr.Caller{Kind: reasonr.CallerUser, Actor: 1, Subject: id, Roles: []string{"admin"}}
	var f reasonr.Features
	if n := testing.AllocsPerRun(100, func() { f = r.Features("acme", s, &c, a) }); n != 0 {
		t.Errorf("allocations: %v", n)
	}
	if got := fmtNames(names(s, f)); got != "[Sales.actor Sales.role]" {
		t.Errorf("flags: %s", got)
	}
}

func TestResolve(t *testing.T) {
	s := schema()
	for in, want := range map[string]string{"Sales.role": "Sales.role", "role": "Sales.role", "nope": ""} {
		if got := features.Resolve(s, in); got != want {
			t.Errorf("Resolve(%q) = %q, want %q", in, got, want)
		}
	}
}
