package server

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"

	"github.com/tycheg/reasonr-engine/internal/config"
	"github.com/tycheg/reasonr-engine/internal/testrun"
)

// demoCommand is reasonrd demo list|load (RFC §27.5): the model's demo
// scenarios — tests in *.demo.rr files with a single given section — fill
// a tenant with demo: true by public commands through the API, as the api
// driver does.
//
//	reasonrd demo list [module]
//	reasonrd demo load "<scenario>" --tenant demo (--url address | --config engine.yaml)
//	    [--extension name] [--seed 1] [--repeat 1] [--parallel 1] [module]
//
// With --config the engine starts in process from the configuration (its
// tenant must have demo: true) and stops after loading; with --url — a
// server already running.
func demoCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "list" && args[0] != "load" {
		fmt.Fprintln(stderr, "reasonrd demo list [module] | demo load \"<scenario>\" --tenant demo (--url address | --config engine.yaml) [module]")
		return 2
	}
	fl := flag.NewFlagSet("reasonrd demo "+args[0], flag.ContinueOnError)
	fl.SetOutput(stderr)
	tenant := fl.String("tenant", "", "a tenant with demo: true")
	base := fl.String("url", "", "the address of a running server")
	cfgPath := fl.String("config", "", "the engine configuration: start the engine in process")
	ext := fl.String("extension", "", "the tenant's model: an extension (by default — from --config, otherwise the base one)")
	seed := fl.Uint64("seed", 0, "the faker seed; run i takes seed+i; 0 — from the scenario name")
	repeat := fl.Int("repeat", 1, "how many times to run the scenario")
	parallel := fl.Int("parallel", 1, "runs at once")
	rest := args[1:]
	var name string
	if args[0] == "load" && len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		name, rest = rest[0], rest[1:]
	}
	if err := fl.Parse(rest); err != nil {
		return 2
	}
	dir := "."
	if fl.NArg() > 0 {
		dir = fl.Arg(0)
	}
	model := strings.TrimPrefix(*ext, "base")
	var cfg *config.Config
	if *cfgPath != "" {
		var err error
		if cfg, err = loadConfig(*cfgPath); err != nil {
			fmt.Fprintf(stderr, "reasonrd demo: %v\n", err)
			return 2
		}
		if m := cfg.Tenants[*tenant]; *ext == "" && m != "" && m != "base" {
			model = m
		}
	}
	modelExt := model
	if modelExt == "base" {
		modelExt = ""
	}
	mach, err := reasonr.Build(dir, reasonr.Options{Extension: modelExt, Test: true})
	if errors.As(err, new(*reasonr.Errors)) {
		fmt.Fprintln(stderr, "reasonrd demo: the model has errors — reasonr check")
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "reasonrd demo: %v\n", err)
		return 2
	}
	scenarios := demoScenarios(mach)
	if args[0] == "list" {
		for _, s := range scenarios {
			fmt.Fprintln(stdout, s)
		}
		return 0
	}
	switch {
	case name == "" || *tenant == "":
		fmt.Fprintln(stderr, "reasonrd demo load: set a scenario and --tenant")
		return 2
	case (*base == "") == (*cfgPath == ""):
		fmt.Fprintln(stderr, "reasonrd demo load: set the server --url or the engine --config")
		return 2
	case cfg != nil && !cfg.Demo[*tenant]:
		fmt.Fprintf(stderr, "reasonrd demo load: tenant %q in %s has no demo: true\n", *tenant, *cfgPath)
		return 2
	case !contains(scenarios, name):
		fmt.Fprintf(stderr, "reasonrd demo load: no scenario %q; there are: %s\n", name, strings.Join(scenarios, ", "))
		return 2
	}
	if *cfgPath != "" {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Fprintf(stderr, "reasonrd demo: %v\n", err)
			return 1
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		srvCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() {
			defer close(done)
			serve(srvCtx, []string{"--config", *cfgPath, "--model", dir, "--listen", addr}, stderr, nil)
		}()
		defer func() { cancel(); <-done }()
		*base = "http://" + addr
		if !waitHealthy(*base, done) {
			fmt.Fprintln(stderr, "reasonrd demo: the engine did not start")
			return 1
		}
	}
	start := time.Now()
	var mu sync.Mutex
	failed := 0
	sem := make(chan struct{}, max(*parallel, 1))
	var wg sync.WaitGroup
	for i := range max(*repeat, 1) {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			d := testrun.NewDemoDriver(*base, *tenant, mach.Schema())
			res, err := mach.RunScenario(ctx, name, d, reasonr.TestOptions{Seed: *seed + uint64(i)})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				failed++
				fmt.Fprintf(stderr, "run %d: %v\n", i+1, err)
			case len(res.Failures) > 0:
				failed++
				for _, f := range res.Failures {
					fmt.Fprintf(stderr, "run %d: %s:%d: %s\n", i+1, f.File, f.Line, testrun.Message(&f))
				}
			}
		}()
	}
	wg.Wait()
	if failed > 0 {
		fmt.Fprintf(stderr, "reasonrd demo load: %d of %d runs failed\n", failed, max(*repeat, 1))
		return 1
	}
	fmt.Fprintf(stdout, "%q: %d runs in tenant %s in %s\n", name, max(*repeat, 1), *tenant, time.Since(start).Round(time.Millisecond))
	return 0
}

// demoScenarios are the names of the tests of the model's *.demo.rr files in alphabetical order.
func demoScenarios(m *reasonr.Model) []string {
	var out []string
	for _, sc := range m.Scenarios() {
		if sc.Kind == reasonr.ScenarioDemo {
			out = append(out, sc.Name)
		}
	}
	sort.Strings(out)
	return out
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// waitHealthy waits for /healthz of the server base up to 60 s or until it exits.
func waitHealthy(base string, done <-chan struct{}) bool {
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if resp, err := http.Get(base + "/healthz"); err == nil {
			resp.Body.Close()
			return true
		}
		select {
		case <-done:
			return false
		default:
		}
	}
	return false
}
