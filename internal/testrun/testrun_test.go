package testrun

import (
	"bytes"
	"context"
	"path/filepath"
	"regexp"
	"testing"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/golden"
	"github.com/tycheg/reasonr-engine/internal/testmodels"
)

func load(t testing.TB, dir, tenant string) *reasonr.Model {
	t.Helper()
	m, err := reasonr.Build(dir, reasonr.Options{Extension: tenant, Test: true})
	if err != nil {
		t.Fatalf("%s: %v", dir, err)
	}
	return m
}

// TestExamples: the RFC example tests pass on the base model and on a
// tenant, and execution is deterministic.
func TestExamples(t *testing.T) {
	for _, c := range []struct{ dir, tenant string }{
		{"taxi", ""}, {"taxi", "acme"}, {"bank", ""},
	} {
		m := load(t, testmodels.Dir(c.dir), c.tenant)
		rep, err := Run(context.Background(), m, Options{CheckDeterminism: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(rep.Tests) == 0 {
			t.Errorf("%s %s: no tests", c.dir, c.tenant)
		}
		if rep.Failed() > 0 {
			var b bytes.Buffer
			_ = WriteText(&b, rep, false)
			t.Errorf("%s %s:\n%s", c.dir, c.tenant, b.String())
		}
	}
}

// TestBroken: "broken" tests fail with the expected messages; the golden
// files are the text reports (with and without -v) and the JSON one.
func TestBroken(t *testing.T) {
	m := load(t, filepath.Join("testdata", "broken"), "")
	rep, err := Run(context.Background(), m, Options{CheckDeterminism: true})
	if err != nil {
		t.Fatal(err)
	}
	rep.Module = "broken"
	for i := range rep.Tests {
		rep.Tests[i].Duration = 0 // the time is not compared in the golden file
	}
	if rep.Failed() != len(rep.Tests)-2 {
		t.Errorf("%d of %d failed, only the last two tests should pass", rep.Failed(), len(rep.Tests))
	}
	var text, short, js bytes.Buffer
	if err := WriteText(&text, rep, true); err != nil {
		t.Fatal(err)
	}
	if err := WriteText(&short, rep, false); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(&js, []*Report{rep}, true); err != nil {
		t.Fatal(err)
	}
	golden.Check(t, filepath.Join("testdata", "broken.txt"), text.Bytes())
	golden.Check(t, filepath.Join("testdata", "broken.short.txt"), short.Bytes())
	golden.Check(t, filepath.Join("testdata", "broken.json"), js.Bytes())
}

func TestRunFilter(t *testing.T) {
	m := load(t, filepath.Join("testdata", "broken"), "")
	rep, err := Run(context.Background(), m, Options{Run: regexp.MustCompile("Passes")})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Tests) != 1 || !rep.Tests[0].Passed() {
		t.Fatalf("tests: %+v", rep.Tests)
	}
}

// BenchmarkTaxi runs every taxi test (the target: < 50 ms).
func BenchmarkTaxi(b *testing.B) {
	m := load(b, testmodels.Dir("taxi"), "")
	for range b.N {
		if _, err := Run(context.Background(), m, Options{}); err != nil {
			b.Fatal(err)
		}
	}
}
