package testrun

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/kind"
)

// Env is what the core lacks: a UI driver and a test mode server.
type Env struct {
	// Web is the web driver (port.TestDriver of the web module, linked by
	// the binary) or nil: the core does not depend on a browser.
	Web kind.TestDriver
	// Serve starts a test mode server of the module module with the
	// configuration config ("" — everything in memory) for the api and web
	// drivers without --url; stop stops it and returns the server log. nil
	// — --url is needed.
	Serve func(ctx context.Context, module, config string) (url string, stop func() string, err error)
}

// Drivers are the --driver drivers.
var Drivers = []string{"memory", "api", "mcp", "web", "mobile"}

// Command is reasonrd test [--driver memory,api,web] [--url address |
// --config engine.yaml] [--extension name] [--run regexp] [--format
// text|json] [-v] [--check-determinism] [--parallel N]
// [path[:line] ...].
func Command(args []string, stdout, stderr io.Writer, env Env) int {
	fl := flag.NewFlagSet("test", flag.ContinueOnError)
	fl.SetOutput(stderr)
	ext := fl.String("extension", "", "only the tests of the model with the extension (the base ones except exclude test, and the extension's) or base — only the base one; by default — every model")
	runRe := fl.String("run", "", "only the tests whose name matches the regular expression")
	format := fl.String("format", "text", "output format: text or json")
	verbose := fl.Bool("v", false, "print every test and its steps: errors, notifications, external requests")
	determinism := fl.Bool("check-determinism", false, "run each command twice and compare the units of work (the memory driver)")
	driverList := fl.String("driver", "memory", "comma-separated drivers: memory, api, web, mobile; a test runs on them in order")
	serverURL := fl.String("url", "", "the test mode server address for api and web; without it the server starts in process")
	cfgPath := fl.String("config", "", "the configuration of the server in process (engine.yaml); by default everything is in memory")
	parallel := fl.Int("parallel", runtime.NumCPU(), "tests at once")
	serveQueue := fl.Bool("serve", false, "coordinator: a test queue for reasonrd test-worker workers")
	listen := fl.String("listen", ":7070", "the coordinator address (--serve)")
	affected := fl.Bool("affected", false, "only the tests a change relative to --base may affect (reasonr impact)")
	base := fl.String("base", "origin/main", "the base version in git for --affected")
	seed := fl.Uint64("seed", 0, "the faker seed in tests and demos; 0 — from the test name")
	impactPath := fl.String("impact", "", "only the tests from a reasonr impact --format json report (instead of --affected)")
	if env.Web != nil {
		env.Web.Flags(fl)
	}
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "reasonr: unknown format %q\n", *format)
		return 2
	}
	drivers, code := parseDrivers(*driverList, env, stderr)
	if code != 0 {
		return code
	}
	remote := slices.ContainsFunc(drivers, func(d string) bool { return d != "memory" })
	switch {
	case !remote && *serverURL != "":
		fmt.Fprintln(stderr, "reasonr: --url is for the api and web drivers")
		return 2
	case *serverURL != "" && *cfgPath != "":
		fmt.Fprintln(stderr, "reasonr: --config is for a server in process: without --url")
		return 2
	case !remote && *cfgPath != "":
		fmt.Fprintln(stderr, "reasonr: --config is for the api and web drivers")
		return 2
	case remote && *serverURL == "" && env.Serve == nil:
		fmt.Fprintln(stderr, "reasonr: set the --url of a test mode server")
		return 2
	case *determinism && !slices.Contains(drivers, "memory"):
		fmt.Fprintln(stderr, "reasonr: --check-determinism is only for the memory driver")
		return 2
	}
	if slices.Contains(drivers, "web") {
		defer env.Web.Close()
	}
	opts := Options{CheckDeterminism: *determinism, Seed: *seed}
	if *runRe != "" {
		re, err := regexp.Compile(*runRe)
		if err != nil {
			fmt.Fprintf(stderr, "reasonr: --run: %v\n", err)
			return 2
		}
		opts.Run = re
	}
	opts.Parallel = *parallel // each test on its own engine or in its own tenant

	args = fl.Args()
	if len(args) == 0 {
		args = []string{"."}
	}
	ctx := context.Background()
	if *serveQueue {
		return coordinate(ctx, args, drivers, *ext, opts, *listen, *format, *verbose, stdout, stderr)
	}
	var reports []*Report
	for _, arg := range args {
		dirs, only, err := target(arg)
		if err != nil {
			fmt.Fprintf(stderr, "reasonr: %v\n", err)
			return 2
		}
		found := false
		for _, dir := range dirs {
			mod, err := reasonr.Load(dir)
			if err != nil {
				fmt.Fprintf(stderr, "reasonr: %v\n", err)
				return 2
			}
			url, stop := *serverURL, func() string { return "" }
			if remote && url == "" {
				if url, stop, err = env.Serve(ctx, dir, *cfgPath); err != nil {
					fmt.Fprintf(stderr, "reasonr: %s: test mode server: %v\n", dir, err)
					return 2
				}
			}
			var selectFor func(ext string) map[string]bool
			if *affected || *impactPath != "" {
				imp, err := loadImpact(dir, *base, *impactPath)
				if err != nil {
					stop()
					fmt.Fprintf(stderr, "reasonr: --affected: %v\n", err)
					return 2
				}
				web := slices.Contains(drivers, "web")
				selectFor = func(ext string) map[string]bool { return imp.Selected(ext, web) }
				if *format == "text" {
					writeImpact(stdout, imp, web)
				}
			}
			reps, c := runModule(ctx, mod, dir, drivers, url, *ext, only, selectFor, opts, env, stderr)
			if log := stop(); c == 2 && log != "" {
				fmt.Fprintf(stderr, "server log:\n%s", log)
			}
			if c == 2 {
				return 2
			}
			code = max(code, c)
			for _, rep := range reps {
				found = found || len(rep.Tests) > 0
				if *format == "text" {
					if err := WriteText(stdout, rep, *verbose); err != nil {
						fmt.Fprintf(stderr, "reasonr: %v\n", err)
						return 2
					}
				}
			}
			reports = append(reports, reps...)
		}
		if only != nil && !found && code == 0 {
			fmt.Fprintf(stderr, "reasonr: %s: no test\n", arg)
			return 2
		}
	}
	if *format == "json" {
		if err := WriteJSON(stdout, reports, *verbose); err != nil {
			fmt.Fprintf(stderr, "reasonr: %v\n", err)
			return 2
		}
	}
	return code
}

// parseDrivers parses --driver: names from Drivers without repeats.
func parseDrivers(list string, env Env, stderr io.Writer) ([]string, int) {
	var out []string
	for _, d := range strings.Split(list, ",") {
		d = strings.TrimSpace(d)
		switch {
		case !slices.Contains(Drivers, d):
			fmt.Fprintf(stderr, "reasonr: unknown driver %q (%s)\n", d, strings.Join(Drivers, ", "))
			return nil, 2
		case slices.Contains(out, d):
			fmt.Fprintf(stderr, "reasonr: driver %s is given twice\n", d)
			return nil, 2
		case d == "mobile":
			fmt.Fprintln(stderr, "reasonr: there is no mobile driver yet")
			return nil, 2
		case d == "web" && env.Web == nil:
			fmt.Fprintln(stderr, "reasonr: --driver web needs a binary that links the web driver (github.com/tycheg/reasonr-web/driver), as reasonrd does")
			return nil, 2
		}
		out = append(out, d)
	}
	return out, 0
}

// target parses a command path: a directory (the modules in it) or a *.rr
// file with an optional line (file:line) — the file's module and a test choice.
func target(arg string) (dirs []string, only *Location, err error) {
	path, line := arg, 0
	if i := strings.LastIndex(arg, ":"); i > 0 {
		if n, err := strconv.Atoi(arg[i+1:]); err == nil && n > 0 {
			path, line = arg[:i], n
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	if info.IsDir() {
		if line > 0 {
			return nil, nil, fmt.Errorf("%s: a line is only for a *.test.rr file", arg)
		}
		if dirs, err = reasonr.FindModules(path); err == nil && len(dirs) == 0 {
			err = fmt.Errorf("%s has no *.rr files", path)
		}
		return dirs, nil, err
	}
	if !strings.HasSuffix(path, ".rr") {
		return nil, nil, fmt.Errorf("%s: not a *.rr file", path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, err
	}
	// The module is the file's directory, for project parts — the directory above them.
	dir, _ := reasonr.FileModule(path)
	return []string{dir}, &Location{File: abs, Line: line}, nil
}

// runModule runs the tests of the models of the module mod on the drivers;
// url is the server for api and web; selectFor gives the tests of a model
// (--affected), nil — all. Code 1 — failures or model errors, 2 — a fault
// (the message is printed).
func runModule(ctx context.Context, mod *reasonr.Module, dir string, drivers []string, url, ext string, only *Location,
	selectFor func(ext string) map[string]bool, opts Options, env Env, stderr io.Writer) ([]*Report, int) {
	code := 0
	var reports []*Report
	for _, t := range Select(mod, ext) {
		m, err := mod.Build(reasonr.Options{Extension: t, Test: true})
		var errs *reasonr.Errors
		if errors.As(err, &errs) {
			// Model errors go to stderr: the tests do not run.
			stderr.Write(errs.Text)
			code = 1
			continue
		}
		if err != nil {
			fmt.Fprintf(stderr, "reasonr: %s: %v\n", dir, err)
			return nil, 2
		}
		model := cmp.Or(t, Base)
		runOpts := opts
		runOpts.Only = only
		if selectFor != nil {
			runOpts.Select = selectFor(t)
		}
		var finish []func(*Report)
		var code2 bool
		runOpts.Drivers, finish, code2 = modelDrivers(ctx, m, model, drivers, url, env, stderr, dir)
		if code2 {
			return nil, 2
		}
		rep, err := Run(ctx, m, runOpts)
		if err != nil {
			fmt.Fprintf(stderr, "reasonr: %s: %v\n", dir, err)
			return nil, 2
		}
		rep.Module = dir
		for _, f := range finish {
			f(rep)
		}
		if only != nil && len(rep.Tests) == 0 {
			continue // this model does not have the chosen test
		}
		if rep.Failed() > 0 {
			code = 1
		}
		reports = append(reports, rep)
	}
	return reports, code
}

// loadImpact is the test selection of the module dir: from the report file
// path or by test impact analysis relative to base.
func loadImpact(dir, base, path string) (*reasonr.ImpactReport, error) {
	if path == "" {
		return reasonr.Impact(dir, base)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rep := &reasonr.ImpactReport{}
	if err := json.Unmarshal(data, rep); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return rep, nil
}

// writeImpact prints the --affected test selection: how many tests of each
// model run and why.
func writeImpact(w io.Writer, imp *reasonr.ImpactReport, web bool) {
	if imp.Full {
		fmt.Fprintf(w, "full run: %s\n", strings.Join(imp.Reasons, "; "))
		return
	}
	for _, a := range imp.Models {
		name := cmp.Or(a.Model, "base")
		if web && a.UI {
			fmt.Fprintf(w, "%s: the interface changed — all %d tests\n", name, a.Total)
			continue
		}
		fmt.Fprintf(w, "%s: running %d of %d tests, %d skipped\n", name, len(a.Tests), a.Total, len(a.Skipped))
	}
}

// modelDrivers prepares the run drivers of the model m (model is base or
// an extension) on the server url; finish completes the UI drivers with
// the report; failed — a fault (the message is printed).
func modelDrivers(ctx context.Context, m *reasonr.Model, model string, drivers []string, url string,
	env Env, stderr io.Writer, dir string) (out []Driver, finish []func(*Report), failed bool) {
	for _, name := range drivers {
		switch name {
		case "memory":
			out = append(out, Driver{Name: name})
		case "api", "mcp":
			newDriver := NewHTTPDriver
			if name == "mcp" {
				newDriver = NewMCPDriver
			}
			out = append(out, Driver{Name: name, New: func(ctx context.Context, _ int) (reasonr.Driver, func(), error) {
				d, err := newDriver(ctx, url, model, m.Schema())
				if err != nil {
					return nil, nil, err
				}
				return d, func() { _ = d.Close(context.WithoutCancel(ctx)) }, nil
			}})
		case "web":
			api := func(ctx context.Context) (kind.TestAPI, error) { return NewHTTPDriver(ctx, url, model, m.Schema()) }
			runs, err := env.Web.Model(ctx, m, model, url, api)
			if err != nil {
				fmt.Fprintf(stderr, "reasonr: %s: %v\n", dir, err)
				return nil, nil, true
			}
			out = append(out, Driver{Name: name, New: runs.New, Runs: runs.Runs})
			finish = append(finish, func(rep *Report) {
				if runs.Uncovered != nil {
					rep.Uncovered = append([]string{}, runs.Uncovered()...)
				}
			})
		}
	}
	return out, finish, false
}

// Base is the base model in --extension and in reports.
const Base = "base"

// Select lists the models of the module that the --extension flag
// chooses: "" — the base model and every extension, Base — the base one,
// otherwise one extension. "" in the result is the base model.
func Select(mod *reasonr.Module, ext string) []string {
	switch ext {
	case "":
		return append([]string{""}, mod.Extensions()...)
	case Base:
		return []string{""}
	}
	return []string{ext}
}
