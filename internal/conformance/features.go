package conformance

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/internal/rows"
	"github.com/tycheg/reasonr-engine/port"
)

// testFeatures: the feature flag rollout (RFC §23.4) — switching on by
// level, a repeat without duplicates, switching off a level and every
// level, the journal in order. Skipped if the store does not keep feature flags.
func testFeatures(t *testing.T, sf Factory) {
	p := kernel.NewPrograms()
	p.Set(T1, Machine(t))
	p.Set(T2, Machine(t))
	st := sf(t, p)
	fs, ok := rows.As[port.FeatureStore](st.Unit)
	if !ok {
		t.Skip("the store does not keep feature flags")
	}
	ctx := context.Background()
	scopes := []port.FeatureScope{
		{},
		{Tenant: T1},
		{Tenant: T1, Role: "Shop.admin"},
		{Tenant: T1, Actor: "Shop.Customer:7"},
		{Actor: "Shop.Customer:8"},
	}
	at := Start
	change := func(c port.FeatureChange) {
		t.Helper()
		at = at.Add(time.Minute)
		c.At, c.By = at, "tester"
		if err := fs.ChangeFeature(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	for _, sc := range scopes {
		change(port.FeatureChange{Feature: "Shop.tips", On: true, Scope: sc})
	}
	change(port.FeatureChange{Feature: "Shop.tips", On: true, Scope: port.FeatureScope{Tenant: T1}}) // a repeat
	change(port.FeatureChange{Feature: "Shop.bonus", On: true, Scope: port.FeatureScope{Tenant: T2}})
	rules := func() []port.FeatureRule {
		t.Helper()
		got, err := fs.FeatureRules(ctx)
		if err != nil {
			t.Fatal(err)
		}
		slices.SortFunc(got, func(a, b port.FeatureRule) int {
			if a.Feature != b.Feature {
				return compare(a.Feature, b.Feature)
			}
			return compare(a.Scope.String(), b.Scope.String())
		})
		return got
	}
	if got := rules(); len(got) != len(scopes)+1 {
		t.Fatalf("rules: %+v", got)
	}
	change(port.FeatureChange{Feature: "Shop.tips", Scope: port.FeatureScope{Tenant: T1, Role: "Shop.admin"}})
	change(port.FeatureChange{Feature: "Shop.tips", Scope: port.FeatureScope{Tenant: T2}}) // not on: nothing
	got := rules()
	if len(got) != len(scopes) {
		t.Fatalf("after switching the role off: %+v", got)
	}
	for _, r := range got {
		if r.Scope.Role != "" {
			t.Fatalf("the role is left: %+v", got)
		}
	}
	change(port.FeatureChange{Feature: "Shop.tips", Everywhere: true})
	if got := rules(); len(got) != 1 || got[0] != (port.FeatureRule{Feature: "Shop.bonus", Scope: port.FeatureScope{Tenant: T2}}) {
		t.Fatalf("after switching off everywhere: %+v", got)
	}
	log, err := fs.FeatureLog(ctx, "Shop.tips")
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != len(scopes)+4 {
		t.Fatalf("journal: %d entries, want %d: %+v", len(log), len(scopes)+4, log)
	}
	for i, c := range log {
		if c.Feature != "Shop.tips" || c.By != "tester" || i > 0 && !log[i-1].At.Before(c.At) {
			t.Fatalf("entry %d: %+v", i, c)
		}
	}
	if last := log[len(log)-1]; last.On || !last.Everywhere {
		t.Errorf("the last entry: %+v", last)
	}
	if first := log[0]; !first.On || !first.Scope.All() || !first.At.Equal(Start.Add(time.Minute)) {
		t.Errorf("the first entry: %+v", first)
	}
	all, err := fs.FeatureLog(ctx, "")
	if err != nil || len(all) != len(log)+1 {
		t.Errorf("the whole journal: %d entries, %v", len(all), err)
	}
}

func compare(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
