// Package perf runs performance tests (RFC §27.6): a *.perf.rr test fills
// the tenant with its given section once, then the user path when runs
// with many virtual users on an open-model schedule (paths start at the
// given rate rather than in a "closed loop": server latencies do not lower
// the load), and each API call is measured. The interpreter evaluates the
// then checks — boolean expressions over stats and baseline — on the
// collected statistics.
package perf

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/testrun"
)

// Options are the run profile.
type Options struct {
	Base     string        // the test mode server
	Model    string        // base or an extension
	Rate     float64       // paths per second
	Duration time.Duration // the load after the ramp-up
	Ramp     time.Duration // a linear ramp-up from 0 to Rate
	Seed     uint64        // the faker seed; path i — Seed+i
	// MaxInFlight is the number of paths at once; beyond it a path does not
	// start and counts as a fault (the generator or the server cannot keep up).
	MaxInFlight int
	Baseline    *Report // the baseline for checks with baseline; nil — such checks are skipped
}

// Stats are call statistics: latencies, the path rate, the number of
// calls, faults (5xx, a transport fault) and business errors; Methods — by method.
type Stats struct {
	P50      Duration          `json:"p50"`
	P90      Duration          `json:"p90"`
	P95      Duration          `json:"p95"`
	P99      Duration          `json:"p99"`
	Max      Duration          `json:"max"`
	Rate     float64           `json:"rate"`
	Calls    int               `json:"calls"`
	Errors   int               `json:"errors"`
	Rejected int               `json:"rejected"`
	Methods  map[string]*Stats `json:"methods,omitempty"`
}

// Duration is a duration in JSON as a string: "12.5ms".
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Duration(d).String() + `"`), nil
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	v, err := time.ParseDuration(strings.Trim(string(b), `"`))
	*d = Duration(v)
	return err
}

// Check is a then check and its outcome.
type Check struct {
	Expr    string `json:"expr"`
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped,omitempty"` // a baseline is needed but there is none
	Error   string `json:"error,omitempty"`
}

// Report is a run report; it is also the baseline (--baseline) of the next one.
type Report struct {
	Version  int      `json:"version"`
	Test     string   `json:"test"`
	Kind     string   `json:"kind"`
	Rate     float64  `json:"rate"` // the given rate (for capacity — the limit found)
	Duration Duration `json:"duration"`
	Paths    int      `json:"paths"` // paths completed
	Stats    Stats    `json:"stats"`
	Checks   []Check  `json:"checks"`
	// Steps are the capacity steps: the rate and whether the checks passed.
	Steps []Step `json:"steps,omitempty"`
}

// Step is a capacity search step.
type Step struct {
	Rate  float64 `json:"rate"`
	OK    bool    `json:"ok"`
	Stats Stats   `json:"stats"`
}

// Passed reports whether every check that was not skipped passed.
func (r *Report) Passed() bool {
	for _, c := range r.Checks {
		if !c.OK && !c.Skipped {
			return false
		}
	}
	return true
}

// Tests returns the model's performance tests (the *.perf.rr files) in name order.
func Tests(m *reasonr.Model) []string {
	var out []string
	for _, sc := range m.Scenarios() {
		if sc.Kind == reasonr.ScenarioPerf {
			out = append(out, sc.Name)
		}
	}
	sort.Strings(out)
	return out
}

// Runner runs a performance test on a server.
type Runner struct {
	mach   *reasonr.Model
	test   string
	opts   Options
	tenant string
	client *http.Client
}

// Prepare fills the tenant: an ephemeral server tenant and the test's given steps.
func Prepare(ctx context.Context, mach *reasonr.Model, test string, opts Options) (*Runner, error) {
	d, err := testrun.NewHTTPDriver(ctx, opts.Base, cmpOr(opts.Model, testrun.Base), mach.Schema())
	if err != nil {
		return nil, err
	}
	res, err := mach.RunScenario(ctx, test, d, reasonr.TestOptions{Seed: opts.Seed, Phase: reasonr.PhaseGiven})
	if err != nil {
		return nil, err
	}
	if len(res.Failures) > 0 {
		f := &res.Failures[0]
		return nil, fmt.Errorf("given: %s:%d: %s", f.File, f.Line, testrun.Message(f))
	}
	if opts.MaxInFlight <= 0 {
		opts.MaxInFlight = 10_000
	}
	tr := &http.Transport{MaxIdleConns: opts.MaxInFlight, MaxIdleConnsPerHost: opts.MaxInFlight, IdleConnTimeout: time.Minute}
	return &Runner{mach: mach, test: test, opts: opts, tenant: d.Tenant(),
		client: &http.Client{Transport: tr, Timeout: 30 * time.Second}}, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// recorder holds the call measurements.
type recorder struct {
	mu       sync.Mutex
	all      []time.Duration
	byMethod map[string][]time.Duration
	errors   map[string]int
	rejected map[string]int
}

func (r *recorder) observe(method string, d time.Duration, status int, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.all = append(r.all, d)
	r.byMethod[method] = append(r.byMethod[method], d)
	switch {
	case err != nil || status >= 500 || status == 0:
		r.errors[method]++
	case status != http.StatusOK:
		r.rejected[method]++
	}
}

// Load runs the when path at the rate rate during ramp+duration and returns
// the statistics and the number of paths.
func (r *Runner) Load(ctx context.Context, rate float64, ramp, duration time.Duration, seedFrom uint64) (Stats, int) {
	rec := &recorder{byMethod: map[string][]time.Duration{}, errors: map[string]int{}, rejected: map[string]int{}}
	var paths, inflight atomic.Int64
	var dropped atomic.Int64
	var wg sync.WaitGroup
	start := time.Now()
	total := ramp + duration
	next := time.Duration(0)
	for i := uint64(0); ; i++ {
		// The moment of the next path: the rate grows linearly over ramp.
		cur := rate
		if ramp > 0 && next < ramp {
			cur = math.Max(rate*float64(next)/float64(ramp), rate/100)
		}
		next += time.Duration(float64(time.Second) / cur)
		if next >= total || ctx.Err() != nil {
			break
		}
		if wait := time.Until(start.Add(next)); wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
			}
		}
		if inflight.Load() >= int64(r.opts.MaxInFlight) {
			dropped.Add(1)
			continue
		}
		inflight.Add(1)
		wg.Add(1)
		go func(seed uint64) {
			defer func() { inflight.Add(-1); wg.Done() }()
			d := testrun.NewPerfDriver(r.opts.Base, r.tenant, r.mach.Schema(), r.client, rec.observe)
			res, err := r.mach.RunScenario(ctx, r.test, d, reasonr.TestOptions{Seed: seed, Phase: reasonr.PhaseWhen})
			if err == nil && !faulted(res) {
				paths.Add(1)
			}
		}(r.opts.Seed + seedFrom + i)
	}
	wg.Wait()
	// The path rate over the load time; the ramp-up counts as half on average.
	effective := time.Since(start) - ramp/2
	st := rec.stats()
	st.Errors += int(dropped.Load())
	if effective > 0 {
		st.Rate = math.Round(float64(paths.Load())/effective.Seconds()*10) / 10
	}
	return st, int(paths.Load())
}

// faulted reports whether a path broke off with a fault (not a step's business error).
func faulted(res *reasonr.TestResult) bool {
	for _, f := range res.Failures {
		if f.Kind == reasonr.FailFault {
			return true
		}
	}
	return false
}

func (r *recorder) stats() Stats {
	r.mu.Lock()
	defer r.mu.Unlock()
	st := summarize(r.all)
	st.Methods = map[string]*Stats{}
	for m, ds := range r.byMethod {
		ms := summarize(ds)
		ms.Errors, ms.Rejected = r.errors[m], r.rejected[m]
		st.Errors += ms.Errors
		st.Rejected += ms.Rejected
		st.Methods[m] = &ms
	}
	return st
}

func summarize(ds []time.Duration) Stats {
	if len(ds) == 0 {
		return Stats{}
	}
	s := append([]time.Duration(nil), ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	q := func(p float64) Duration { return Duration(s[min(len(s)-1, int(math.Ceil(p*float64(len(s))))-1)]) }
	return Stats{P50: q(0.50), P90: q(0.90), P95: q(0.95), P99: q(0.99), Max: Duration(s[len(s)-1]), Calls: len(s)}
}

// Checks evaluates the then checks on the statistics st (and the baseline, if any).
func (r *Runner) Checks(ctx context.Context, st Stats) []Check {
	checks := r.mach.PerfChecks(r.test)
	if len(checks) == 0 {
		return nil
	}
	var base *Stats
	if r.opts.Baseline != nil {
		base = &r.opts.Baseline.Stats
	}
	var out []Check
	for i, expr := range checks {
		c := Check{Expr: expr}
		stat := func(key string) (reasonr.Value, error) { return statValue(key, &st, base) }
		ok, err := r.mach.PerfCheck(ctx, r.test, i, false, stat)
		if errors.Is(err, reasonr.ErrNotFound) {
			if base == nil {
				c.Skipped = true
				out = append(out, c)
				continue
			}
			ok, err = r.mach.PerfCheck(ctx, r.test, i, true, stat)
		}
		c.OK = ok && err == nil
		if err != nil {
			c.Error = err.Error()
		}
		out = append(out, c)
	}
	return out
}

// statValue is a statistics field by the key lower.PerfKey.
func statValue(key string, cur, base *Stats) (reasonr.Value, error) {
	parts := strings.SplitN(key, "|", 3)
	if len(parts) != 3 {
		return reasonr.None, fmt.Errorf("perf: key %q", key)
	}
	st := cur
	if parts[0] == "baseline" {
		if base == nil {
			return reasonr.None, errors.New("perf: no baseline run (--baseline)")
		}
		st = base
	}
	if m := parts[1]; m != "" {
		ms := st.Methods[m] // the full name: the /api path has the type's full name and the method
		if ms == nil {
			ms = &Stats{}
		}
		st = ms
	}
	switch parts[2] {
	case "p50":
		return reasonr.Duration(time.Duration(st.P50)), nil
	case "p90":
		return reasonr.Duration(time.Duration(st.P90)), nil
	case "p95":
		return reasonr.Duration(time.Duration(st.P95)), nil
	case "p99":
		return reasonr.Duration(time.Duration(st.P99)), nil
	case "max":
		return reasonr.Duration(time.Duration(st.Max)), nil
	case "rate":
		return reasonr.Float(st.Rate), nil
	case "calls":
		return reasonr.Int(int64(st.Calls)), nil
	case "errors":
		return reasonr.Int(int64(st.Errors)), nil
	case "rejected":
		return reasonr.Int(int64(st.Rejected)), nil
	}
	return reasonr.None, fmt.Errorf("perf: no field stats.%s", parts[2])
}
