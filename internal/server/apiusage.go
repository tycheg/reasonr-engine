package server

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
)

// apiUsageLine is a metric line reasonr_api_calls_total{method="…",version="…"} N.
var apiUsageLine = regexp.MustCompile(`^reasonr_api_calls_total\{method="([^"]*)",version="([^"]*)"\} ([0-9.e+]+)$`)

// apiCommand is reasonrd api usage --url address: calls of the external API
// by method and client version (RFC §31.3.1) from the metrics of an engine
// instance: who is on an old version and whether it can be retired.
func apiCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) < 1 || args[0] != "usage" {
		fmt.Fprintln(stderr, "reasonrd: api usage --url address")
		return 2
	}
	fs := flag.NewFlagSet("api usage", flag.ContinueOnError)
	fs.SetOutput(stderr)
	url := fs.String("url", "http://localhost:8080", "the address of an engine instance with /metrics")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(*url, "/")+"/metrics", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(stderr, "reasonrd: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "reasonrd: /metrics: %s (metrics are switched on in telemetry.metrics)\n", resp.Status)
		return 1
	}
	type row struct {
		method, version string
		calls           float64
	}
	var rows []row
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if m := apiUsageLine.FindStringSubmatch(sc.Text()); m != nil {
			n, _ := strconv.ParseFloat(m[3], 64)
			rows = append(rows, row{m[1], m[2], n})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].version != rows[j].version {
			return rows[i].version < rows[j].version
		}
		return rows[i].method < rows[j].method
	})
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "VERSION\tMETHOD\tCALLS")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%.0f\n", r.version, r.method, r.calls)
	}
	_ = w.Flush()
	return 0
}
