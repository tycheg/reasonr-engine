package server

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tycheg/reasonr-engine/internal/perf"
	"github.com/tycheg/reasonr-engine/internal/testmodels"
)

// TestPerf: reasonrd perf smoke on a test mode server — a false then check
// gives code 1; --baseline catches a 20 % slowdown of OrderTaxi (a proxy in
// front of the server stretches its responses), and without the slowdown
// it passes.
func TestPerf(t *testing.T) {
	if testing.Short() {
		t.Skip("load — not with -short")
	}
	t.Setenv("REASONR_MODULES", testmodels.Modules())
	project := t.TempDir() // the whole taxi project: reasonr.yaml with modules
	if err := os.CopyFS(project, os.DirFS(testmodels.Example("taxi"))); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(project, "src")
	perfFile := filepath.Join(model, "contexts", "Ordering", "peak.perf.rr")
	src, err := os.ReadFile(perfFile)
	if err != nil {
		t.Fatal(err)
	}
	head, _, _ := strings.Cut(string(src), "  then\n")
	write := func(then string) {
		if err := os.WriteFile(perfFile, []byte(head+"  then\n"+then), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	url, stop, err := serveTest(context.Background(), model, "")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	// A proxy: OrderTaxi responses are stretched slow/100 times.
	var slow atomic.Int64
	slow.Store(100)
	target, _ := urlParse(url)
	rp := httputil.NewSingleHostReverseProxy(target)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rp.ServeHTTP(w, r)
		if strings.HasSuffix(r.URL.Path, "/OrderTaxi") {
			time.Sleep(time.Since(start) * time.Duration(slow.Load()-100) / 100)
		}
	}))
	defer proxy.Close()
	smoke := func(extra ...string) (int, []*perf.Report, string) {
		var out, errOut bytes.Buffer
		args := append([]string{"smoke", "--url", proxy.URL, "--duration", "4s", "--rate", "60/s", "--format", "json"}, extra...)
		code := perfCommand(context.Background(), append(args, model), &out, &errOut)
		var reps []*perf.Report
		_ = json.Unmarshal(out.Bytes(), &reps)
		return code, reps, out.String() + errOut.String()
	}

	write("    stats.p95 < 1ns\n")
	if code, _, out := smoke(); code != 1 {
		t.Fatalf("a false check: code %d\n%s", code, out)
	}

	write("    stats.errors == 0\n    stats.of(Customer.OrderTaxi).p50 * 10 <= baseline.of(Customer.OrderTaxi).p50 * 11\n")
	code, reps, out := smoke()
	if code != 0 || len(reps) != 1 || !reps[0].Checks[1].Skipped {
		t.Fatalf("baseline: code %d\n%s", code, out)
	}
	base := filepath.Join(t.TempDir(), "base.json")
	data, _ := json.Marshal(reps)
	if err := os.WriteFile(base, data, 0o644); err != nil {
		t.Fatal(err)
	}
	// A slowdown with a margin over the check threshold (10 %): latencies
	// are fractions of a millisecond, and under the load of a full check the
	// noise is comparable to 20 %.
	slow.Store(150)
	code, reps, out = smoke("--baseline", base)
	if code != 1 || len(reps) != 1 || reps[0].Checks[1].OK {
		got := time.Duration(reps[0].Stats.Methods["Ordering.Customer.OrderTaxi"].P50)
		t.Fatalf("a 50 %% slowdown is not caught (p50 %s): code %d\n%s", got, code, out)
	}
	ratio := float64(reps[0].Stats.Methods["Ordering.Customer.OrderTaxi"].P50) / float64(mustBase(t, base).Stats.Methods["Ordering.Customer.OrderTaxi"].P50)
	if math.IsNaN(ratio) || ratio < 1.1 {
		t.Fatalf("slowdown: p50 ratio %.2f", ratio)
	}
}

func urlParse(s string) (*url.URL, error) { return url.Parse(s) }

func mustBase(t *testing.T, path string) *perf.Report {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var reps []*perf.Report
	if err := json.Unmarshal(data, &reps); err != nil || len(reps) == 0 {
		t.Fatal(err)
	}
	return reps[0]
}
