// Command scale measures the engine at scale (plan 0015, S4): loading the
// model program and running the acceptance tests on the memory driver. The
// model comes from compiler/internal/genmodel (make scale).
//
//	go run ./internal/scale --model dir [--tests N] [--parallel N] [-o report.json] [--budget]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/testrun"
)

// Report is an engine measurement report.
type Report struct {
	Version int `json:"version"`
	CPUs    int `json:"cpus"`
	// Build is building the program from the sources (the compiler), Load
	// is loading the built program into the interpreter, ms; Program is the
	// program's live heap after loading, MB.
	Build   int64 `json:"buildMs"`
	Load    int64 `json:"loadMs"`
	Program int64 `json:"programMB"`
	// Tests is the number of tests run, TestsMs how long it took, Failed the failures.
	Tests   int      `json:"tests"`
	TestsMs int64    `json:"testsMs"`
	Failed  int      `json:"failed"`
	Budget  []string `json:"budget,omitempty"` // the violated budgets
}

func main() {
	dir := flag.String("model", "", "the model directory (genmodel)")
	limit := flag.Int("tests", 0, "how many tests to run (0 — all)")
	parallel := flag.Int("parallel", runtime.NumCPU(), "tests at once")
	out := flag.String("o", "", "the JSON report file (stdout by default)")
	budget := flag.Bool("budget", false, "check the plan 0015 budgets: loading ≤ 10 s, the program ≤ 4 GB, 100,000 tests ≤ 10 min")
	flag.Parse()
	if err := run(*dir, *limit, *parallel, *out, *budget); err != nil {
		fmt.Fprintln(os.Stderr, "scale:", err)
		os.Exit(2)
	}
}

func run(dir string, limit, parallel int, out string, budget bool) error {
	if dir == "" {
		return fmt.Errorf("set --model")
	}
	rep := &Report{Version: 1, CPUs: runtime.NumCPU()}
	heap := func() int64 {
		var ms runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&ms)
		return int64(ms.HeapAlloc)
	}
	start := time.Now()
	before := heap()
	m, err := reasonr.Build(dir, reasonr.Options{Test: true})
	if err != nil {
		return err
	}
	// The build is the whole compiler with the program loaded into the
	// interpreter (reasonr.Build); the compiler measures its phases itself
	// (compiler/internal/scale).
	rep.Build = time.Since(start).Milliseconds()
	rep.Program = (heap() - before) >> 20
	fmt.Fprintf(os.Stderr, "build         %8d ms %6d MB\n", rep.Build, rep.Program)

	names := m.Tests()
	if limit > 0 && limit < len(names) {
		names = names[:limit]
	}
	sel := map[string]bool{}
	for _, n := range names {
		sel[n] = true
	}
	start = time.Now()
	tr, err := testrun.Run(context.Background(), m, testrun.Options{Parallel: parallel, Select: sel})
	if err != nil {
		return err
	}
	rep.TestsMs = time.Since(start).Milliseconds()
	rep.Tests, rep.Failed = len(tr.Tests), tr.Failed()
	fmt.Fprintf(os.Stderr, "tests         %8d ms: %d tests, %d failed, %d threads\n", rep.TestsMs, rep.Tests, rep.Failed, parallel)

	if budget {
		if rep.Load > 10_000 {
			rep.Budget = append(rep.Budget, fmt.Sprintf("program load: %d ms, budget 10,000 ms", rep.Load))
		}
		if rep.Program > 4_096 {
			rep.Budget = append(rep.Budget, fmt.Sprintf("program: %d MB, budget 4096 MB", rep.Program))
		}
		// 100,000 tests in 10 minutes on 8 cores: 48 ms of a core per test.
		if limit := int64(rep.Tests) * 48 / int64(min(parallel, 8)); rep.TestsMs > limit {
			rep.Budget = append(rep.Budget, fmt.Sprintf("tests: %d ms, budget %d ms", rep.TestsMs, limit))
		}
	}
	data, _ := json.MarshalIndent(rep, "", "  ")
	data = append(data, '\n')
	if out != "" {
		err = os.WriteFile(out, data, 0o644)
	} else {
		_, err = os.Stdout.Write(data)
	}
	if err != nil {
		return err
	}
	for _, b := range rep.Budget {
		fmt.Fprintln(os.Stderr, "failed:", b)
	}
	if len(rep.Budget) > 0 || rep.Failed > 0 {
		os.Exit(1)
	}
	return nil
}
