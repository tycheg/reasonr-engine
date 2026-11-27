package testrun

import (
	"context"
	"slices"
	"testing"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/testmodels"
)

// TestImpactMutations checks test impact analysis (RFC §27.4): each
// function of the taxi and bank models is broken in turn (its entry is an
// endless loop up to the iteration limit), and every test that fails from
// that must reach this function in the analysis graph: otherwise the
// analysis would skip a test affected by a change of the function.
func TestImpactMutations(t *testing.T) {
	if testing.Short() {
		t.Skip("a mutation run — not with -short")
	}
	mutants, killed := 0, 0
	defer func() { t.Logf("%d mutants, %d of them failed tests", mutants, killed) }()
	for _, dir := range []string{testmodels.Dir("taxi"), testmodels.Dir("bank")} {
		mod, err := reasonr.Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, ext := range Select(mod, "") {
			m, err := mod.Build(reasonr.Options{Extension: ext, Test: true})
			if err != nil {
				t.Fatalf("%s %s: %v", dir, ext, err)
			}
			tests, err := m.ImpactNodes()
			if err != nil {
				t.Fatal(err)
			}
			err = m.Mutate(func(node string, mutant *reasonr.Model) error {
				rep, err := Run(context.Background(), mutant, Options{Parallel: 8})
				if err != nil {
					return err
				}
				mutants++
				if rep.Failed() > 0 {
					killed++
				}
				for _, tr := range rep.Tests {
					if !tr.Passed() && !slices.Contains(tests[tr.Name], node) {
						t.Errorf("%s [%s]: test %q fails from breaking %s, but the analysis does not select it", dir, ext, tr.Name, node)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}
