package server

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/perf"
)

// perfCommand is reasonrd perf smoke|load|capacity (RFC §27.6): performance
// tests *.perf.rr on a test mode server — started in this process on the
// --config adapters (as --driver api) or at --url.
//
//	reasonrd perf smoke [--config engine.yaml | --url address] [--run test] [--baseline report.json] [--format json] [module]
//	reasonrd perf load --rate 300/s --duration 15m --ramp 2m …
//	reasonrd perf capacity [--rate 10/s] [--step 30s] …
//
// Code 1 means a then check is false (for capacity — already at the initial rate).
func perfCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	defaults := map[string]struct {
		rate     float64
		dur, rmp time.Duration
	}{"smoke": {20, time.Minute, 0}, "load": {100, 15 * time.Minute, 2 * time.Minute}, "capacity": {10, 30 * time.Second, 0}}
	if len(args) == 0 {
		fmt.Fprintln(stderr, "reasonrd perf smoke|load|capacity [flags] [module]")
		return 2
	}
	kind := args[0]
	def, ok := defaults[kind]
	if !ok {
		fmt.Fprintf(stderr, "reasonrd perf: mode %q — smoke, load or capacity\n", kind)
		return 2
	}
	fl := flag.NewFlagSet("reasonrd perf "+kind, flag.ContinueOnError)
	fl.SetOutput(stderr)
	rateText := fl.String("rate", "", "the path rate: 300/s")
	dur := fl.Duration("duration", def.dur, "the load (for capacity — the step duration)")
	ramp := fl.Duration("ramp", def.rmp, "a linear ramp-up to --rate")
	cfg := fl.String("config", "", "the configuration of a test mode server in this process")
	base := fl.String("url", "", "the address of a running test mode server")
	ext := fl.String("extension", "", "the model: an extension (by default — the base one)")
	runRe := fl.String("run", "", "tests whose name matches the regular expression")
	baseline := fl.String("baseline", "", "the report of a previous run (--format json): checks with baseline")
	format := fl.String("format", "text", "report format: text or json")
	seed := fl.Uint64("seed", 1, "the faker seed")
	inflight := fl.Int("max-inflight", 10_000, "paths at once; more is a fault")
	if err := fl.Parse(args[1:]); err != nil {
		return 2
	}
	rate := def.rate
	if *rateText != "" {
		v, err := strconv.ParseFloat(strings.TrimSuffix(*rateText, "/s"), 64)
		if err != nil || v <= 0 {
			fmt.Fprintf(stderr, "reasonrd perf: --rate %q — for example, 300/s\n", *rateText)
			return 2
		}
		rate = v
	}
	dir := "."
	if fl.NArg() > 0 {
		dir = fl.Arg(0)
	}
	m, err := reasonr.Build(dir, reasonr.Options{Extension: *ext, Test: true})
	if errors.As(err, new(*reasonr.Errors)) {
		fmt.Fprintln(stderr, "reasonrd perf: the model has errors — reasonr check")
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "reasonrd perf: %v\n", err)
		return 2
	}
	var re *regexp.Regexp
	if *runRe != "" {
		if re, err = regexp.Compile(*runRe); err != nil {
			fmt.Fprintf(stderr, "reasonrd perf: --run: %v\n", err)
			return 2
		}
	}
	var tests []string
	for _, t := range perf.Tests(m) {
		if re == nil || re.MatchString(t) {
			tests = append(tests, t)
		}
	}
	if len(tests) == 0 {
		fmt.Fprintf(stderr, "reasonrd perf: %s has no *.perf.rr tests\n", dir)
		return 2
	}
	opts := perf.Options{Base: *base, Model: *ext, Rate: rate, Duration: *dur, Ramp: *ramp, Seed: *seed, MaxInFlight: *inflight}
	baselines := map[string]*perf.Report{}
	if *baseline != "" {
		data, err := os.ReadFile(*baseline)
		var reps []*perf.Report
		if err == nil {
			err = json.Unmarshal(data, &reps)
		}
		if err != nil {
			fmt.Fprintf(stderr, "reasonrd perf: --baseline: %v\n", err)
			return 2
		}
		for _, r := range reps {
			baselines[r.Test] = r
		}
	}
	if opts.Base == "" {
		url, stop, err := serveTest(ctx, dir, *cfg)
		if err != nil {
			fmt.Fprintf(stderr, "reasonrd perf: test mode server: %v\n", err)
			return 2
		}
		defer stop()
		opts.Base = url
	}
	code := 0
	var reports []*perf.Report
	for _, t := range tests {
		o := opts
		o.Baseline = baselines[t]
		rep, err := perfRun(ctx, m, t, kind, o, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "reasonrd perf: «%s»: %v\n", t, err)
			return 2
		}
		if !rep.Passed() {
			code = 1
		}
		reports = append(reports, rep)
		if *format == "text" {
			writePerf(stdout, rep)
		}
	}
	if *format == "json" {
		data, _ := json.MarshalIndent(reports, "", "  ")
		fmt.Fprintln(stdout, string(data))
	}
	return code
}

// perfRun runs a test in one mode.
func perfRun(ctx context.Context, m *reasonr.Model, test, kind string, o perf.Options, stderr io.Writer) (*perf.Report, error) {
	r, err := perf.Prepare(ctx, m, test, o)
	if err != nil {
		return nil, err
	}
	rep := &perf.Report{Version: 1, Test: test, Kind: kind, Rate: o.Rate, Duration: perf.Duration(o.Duration)}
	if kind != "capacity" {
		rep.Stats, rep.Paths = r.Load(ctx, o.Rate, o.Ramp, o.Duration, 0)
		rep.Checks = r.Checks(ctx, rep.Stats)
		return rep, nil
	}
	// capacity: steps ×1.5 while the checks hold; the limit is the last step
	// that holds.
	rate, seed := o.Rate, uint64(0)
	for {
		st, paths := r.Load(ctx, rate, 0, o.Duration, seed)
		seed += uint64(paths) + 1
		checks := r.Checks(ctx, st)
		ok := (&perf.Report{Checks: checks}).Passed() && st.Rate >= rate*0.9 // the generator and the server keep up
		fmt.Fprintf(stderr, "step %.0f/s: %.1f paths/s, p95 %s, %d faults — %s\n", rate, st.Rate, time.Duration(st.P95), st.Errors,
			map[bool]string{true: "holds", false: "does not hold"}[ok])
		rep.Steps = append(rep.Steps, perf.Step{Rate: rate, OK: ok, Stats: st})
		if !ok || ctx.Err() != nil {
			rep.Checks = checks
			break
		}
		rep.Rate, rep.Stats, rep.Paths, rep.Checks = rate, st, paths, checks
		rate *= 1.5
	}
	if len(rep.Steps) > 1 {
		// The limit is found: the report is about the last step the system holds.
		last := rep.Steps[len(rep.Steps)-2]
		rep.Rate, rep.Stats = last.Rate, last.Stats
		rep.Checks = r.Checks(ctx, last.Stats)
	}
	return rep, nil
}

// writePerf prints a report as text.
func writePerf(w io.Writer, rep *perf.Report) {
	st := &rep.Stats
	status := "ok"
	if !rep.Passed() {
		status = "FAIL"
	}
	fmt.Fprintf(w, "%s\t%q (%s): %.1f paths/s, %d calls, p50 %s, p95 %s, p99 %s, max %s, %d faults, %d rejected\n",
		status, rep.Test, rep.Kind, st.Rate, st.Calls, time.Duration(st.P50), time.Duration(st.P95), time.Duration(st.P99),
		time.Duration(st.Max), st.Errors, st.Rejected)
	if rep.Kind == "capacity" {
		fmt.Fprintf(w, "\tlimit: %.0f paths/s\n", rep.Rate)
	}
	for _, c := range rep.Checks {
		mark := "ok  "
		switch {
		case c.Skipped:
			mark = "skip"
		case !c.OK:
			mark = "FAIL"
		}
		line := "\t" + mark + " " + c.Expr
		if c.Error != "" {
			line += " — " + c.Error
		}
		fmt.Fprintln(w, line)
	}
}
