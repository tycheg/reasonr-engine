// Package testrun runs a model's acceptance tests (RFC §27, §29): building
// the program, running each test on a new engine and the report for people
// (text) and agents (JSON). The VM runs the steps and then checks
// (reasonr.Model.RunTest), the engine is attached by a driver.
package testrun

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/memory"
	"github.com/tycheg/reasonr-engine/kind"
)

// Options are the run settings.
type Options struct {
	Run              *regexp.Regexp // only the tests with a matching name; nil — all
	CheckDeterminism bool           // run each command twice and compare
	// Backend is the engine store of each test; nil — in memory.
	Backend func(kind.Catalog) memory.Backend
	// Drivers are the drivers of each test's runs in order (--driver);
	// empty — one run in memory.
	Drivers []Driver
	// Parallel is how many tests run at once; 0 — 1.
	Parallel int
	// Only — only the tests declared in the file File, and with Line the
	// test that contains the line Line (reasonrd test file:line); nil — all.
	Only *Location
	// Seed is the faker seed (RFC §27.5); 0 seeds from the test name only.
	Seed     uint64
	prepared *prepared // a ready program (a worker); nil — prepare it
	// Select — only the tests with these names (--affected); nil — all.
	Select map[string]bool
}

// Driver is a driver of test runs (RFC §27.3).
type Driver struct {
	// Name is the driver name in --driver and in the report: memory, api, web.
	Name string
	// New gives the driver of the run run (from 0) of this driver and its
	// release after the run; nil — the in-memory engine. UI drivers run a
	// test k times: in run i a step takes place i.
	New func(ctx context.Context, run int) (reasonr.Driver, func(), error)
	// Runs is how many runs a test needs on this driver; it is asked of the
	// driver of the first run after it. nil — one run.
	Runs func(d reasonr.Driver) int
}

// Location is a place in the source: the absolute file path and a line (0
// — the whole file).
type Location struct {
	File string
	Line int
}

// Report holds the test results of one model.
type Report struct {
	Module    string // the module path for the report; set by the caller
	Extension string // "" — the base model
	Tests     []Test
	// Uncovered are the UI places no step went through (--driver web
	// --coverage); nil — coverage was not counted.
	Uncovered []string
}

// Failed returns the number of failed tests.
func (r *Report) Failed() int {
	n := 0
	for i := range r.Tests {
		if !r.Tests[i].Passed() {
			n++
		}
	}
	return n
}

// Test is a test result and its place in the source.
type Test struct {
	*reasonr.TestResult
	File string
	Line int
	// Duration is the time of all runs of the test on all drivers.
	Duration time.Duration
}

// Run runs the tests of the model m: each on a new in-memory engine or on
// the driver Options.NewDriver.
// error is a build or execution fault outside the tests.
func Run(ctx context.Context, m *reasonr.Model, opts Options) (*Report, error) {
	p := opts.prepared
	if p == nil {
		p = prepare(m)
	}
	mach, pos, files := p.mach, p.pos, p.files
	rep := &Report{Extension: m.Extension()}
	var names []string
	for _, name := range mach.Tests() {
		if (opts.Run == nil || opts.Run.MatchString(name)) && pos[name].in(opts.Only) && (opts.Select == nil || opts.Select[name]) {
			names = append(names, name)
		}
	}
	drivers := opts.Drivers
	if len(drivers) == 0 {
		drivers = []Driver{{Name: "memory"}}
	}
	results := make([]*reasonr.TestResult, len(names))
	errs := make([]error, len(names))
	took := make([]time.Duration, len(names))
	par := max(opts.Parallel, 1)
	sem := make(chan struct{}, par)
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			start := time.Now()
			results[i], errs[i] = runs(ctx, mach, name, drivers, opts)
			took[i] = time.Since(start)
		}()
		if par == 1 {
			wg.Wait()
		}
	}
	wg.Wait()
	for i, name := range names {
		if errs[i] != nil {
			return nil, fmt.Errorf("test %q: %w", name, errs[i])
		}
		res := results[i]
		p := pos[name]
		for i := range res.Failures {
			f := &res.Failures[i]
			if f.Line == 0 {
				f.File, f.Line = p.Filename, p.Line // the step place is unknown — the test place
			} else if name, ok := files[f.File]; ok {
				f.File = name
			}
		}
		rep.Tests = append(rep.Tests, Test{TestResult: res, File: p.Filename, Line: p.Line, Duration: took[i]})
	}
	return rep, nil
}

// runs runs a test on the drivers in order: on each as many times as its
// first run says. Failures carry the driver, failures after the test's
// first run carry the run number (from 1).
func runs(ctx context.Context, mach *reasonr.Model, name string, drivers []Driver, opts Options) (*reasonr.TestResult, error) {
	var out *reasonr.TestResult
	n := 0 // test runs before the current one
	for _, drv := range drivers {
		for run, k := 0, 1; run < k; run, n = run+1, n+1 {
			var d reasonr.Driver
			release := func() {}
			if drv.New == nil {
				// The test clock goes from the current time (RFC §27.5).
				d = newMemoryDriver(memory.New(mach, memory.Options{Now: time.Now().UTC(), CheckDeterminism: opts.CheckDeterminism, Backend: opts.Backend}))
			} else {
				var err error
				if d, release, err = drv.New(ctx, run); err != nil {
					return nil, err
				}
			}
			res, err := mach.RunScenario(ctx, name, d, reasonr.TestOptions{Seed: opts.Seed})
			if run == 0 && err == nil && drv.Runs != nil {
				k = drv.Runs(d)
			}
			release()
			if err != nil {
				return nil, err
			}
			for i := range res.Failures {
				if drv.New != nil {
					res.Failures[i].Driver = drv.Name
				}
				if n > 0 {
					res.Failures[i].Run = n + 1
				}
			}
			if out == nil {
				out = res
				continue
			}
			out.Failures = append(out.Failures, res.Failures...)
		}
	}
	return out, nil
}

type position struct {
	Filename string
	Line     int
	End      int // the last line of the declaration
}

// in reports whether the test matches the choice only.
func (p position) in(only *Location) bool {
	if only == nil {
		return true
	}
	abs, err := filepath.Abs(p.Filename)
	if err != nil || abs != only.File {
		return false
	}
	return only.Line == 0 || p.Line <= only.Line && only.Line <= p.End
}

// prepared is a loaded model program and the test places: a worker
// prepares them once per model, not per test.
type prepared struct {
	mach  *reasonr.Model
	pos   map[string]position
	files map[string]string // the path relative to the module → the path for the report
}

func prepare(m *reasonr.Model) *prepared {
	p := &prepared{mach: m, pos: map[string]position{}, files: m.Files()}
	for _, sc := range m.Scenarios() {
		p.pos[sc.Name] = position{sc.File, sc.Line, sc.End}
	}
	return p
}
