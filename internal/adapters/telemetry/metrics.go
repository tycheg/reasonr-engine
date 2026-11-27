// Package telemetry is the engine telemetry without external dependencies
// (RFC §30–31): logs through log/slog (text or JSON) and port.Metrics
// metrics in the Prometheus text format (pull, /metrics). OTLP is a
// separate module, adapters/otel.
package telemetry

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/tycheg/reasonr-engine/port"
)

// DefaultBuckets are the default histogram bounds, in seconds.
var DefaultBuckets = []float64{0.0005, 0.001, 0.0025, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Prometheus holds metrics in process memory and serves them in the
// Prometheus 0.0.4 text format: Handler serves them at /metrics.
// Registering a metric with the same name again returns the registered one.
type Prometheus struct {
	mu      sync.Mutex
	metrics map[string]*metric
}

var _ port.Metrics = (*Prometheus)(nil)

// NewPrometheus returns an empty metric set.
func NewPrometheus() *Prometheus { return &Prometheus{metrics: map[string]*metric{}} }

type metric struct {
	name, help, kind string
	labels           []string
	buckets          []float64

	mu     sync.Mutex
	series map[string]*series
}

type series struct {
	values []string
	value  float64  // a counter or gauge; the sum for a histogram
	counts []uint64 // a histogram: by bound
	count  uint64   // a histogram: the total
}

func (p *Prometheus) register(name, help, kind string, buckets []float64, labels []string) *metric {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m := p.metrics[name]; m != nil {
		return m
	}
	m := &metric{name: name, help: help, kind: kind, labels: labels, buckets: buckets, series: map[string]*series{}}
	p.metrics[name] = m
	return m
}

// Counter registers a counter.
func (p *Prometheus) Counter(name, help string, labels ...string) port.Counter {
	return counter{p.register(name, help, "counter", nil, labels)}
}

// Histogram registers a histogram; nil buckets — DefaultBuckets.
func (p *Prometheus) Histogram(name, help string, buckets []float64, labels ...string) port.Histogram {
	if buckets == nil {
		buckets = DefaultBuckets
	}
	return histogram{p.register(name, help, "histogram", buckets, labels)}
}

// Gauge registers a gauge.
func (p *Prometheus) Gauge(name, help string, labels ...string) port.Gauge {
	return gauge{p.register(name, help, "gauge", nil, labels)}
}

// at returns the series by label values; called under m.mu.
func (m *metric) at(values []string) *series {
	key := strings.Join(values, "\xff")
	s := m.series[key]
	if s == nil {
		vals := make([]string, len(m.labels))
		copy(vals, values)
		s = &series{values: vals}
		if m.buckets != nil {
			s.counts = make([]uint64, len(m.buckets))
		}
		m.series[key] = s
	}
	return s
}

type counter struct{ m *metric }

func (c counter) Add(delta float64, values ...string) {
	if delta < 0 {
		return // a counter does not decrease
	}
	c.m.mu.Lock()
	c.m.at(values).value += delta
	c.m.mu.Unlock()
}

type gauge struct{ m *metric }

func (g gauge) Set(v float64, values ...string) {
	g.m.mu.Lock()
	g.m.at(values).value = v
	g.m.mu.Unlock()
}

type histogram struct{ m *metric }

func (h histogram) Observe(v float64, values ...string) {
	h.m.mu.Lock()
	s := h.m.at(values)
	for i, b := range h.m.buckets {
		if v <= b {
			s.counts[i]++
		}
	}
	s.count++
	s.value += v
	h.m.mu.Unlock()
}

// Write returns every metric in the Prometheus text format.
func (p *Prometheus) Write() []byte {
	p.mu.Lock()
	names := make([]string, 0, len(p.metrics))
	for n := range p.metrics {
		names = append(names, n)
	}
	p.mu.Unlock()
	sort.Strings(names)
	var b []byte
	for _, n := range names {
		p.mu.Lock()
		m := p.metrics[n]
		p.mu.Unlock()
		b = m.write(b)
	}
	return b
}

func (m *metric) write(b []byte) []byte {
	b = fmt.Appendf(b, "# HELP %s %s\n# TYPE %s %s\n", m.name, escapeHelp(m.help), m.name, m.kind)
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.series))
	for k := range m.series {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s := m.series[k]
		if m.kind != "histogram" {
			b = m.sample(b, m.name, s.values, "", "", s.value)
			continue
		}
		for i, le := range m.buckets {
			b = m.sample(b, m.name+"_bucket", s.values, "le", formatFloat(le), float64(s.counts[i]))
		}
		b = m.sample(b, m.name+"_bucket", s.values, "le", "+Inf", float64(s.count))
		b = m.sample(b, m.name+"_sum", s.values, "", "", s.value)
		b = m.sample(b, m.name+"_count", s.values, "", "", float64(s.count))
	}
	return b
}

func (m *metric) sample(b []byte, name string, values []string, extra, extraValue string, v float64) []byte {
	b = append(b, name...)
	n := 0
	label := func(k, v string) {
		if n == 0 {
			b = append(b, '{')
		} else {
			b = append(b, ',')
		}
		n++
		b = append(b, k...)
		b = append(b, '=')
		b = append(append(append(b, '"'), labelEscaper.Replace(v)...), '"')
	}
	for i, l := range m.labels {
		label(l, values[i])
	}
	if extra != "" {
		label(extra, extraValue)
	}
	if n > 0 {
		b = append(b, '}')
	}
	b = append(b, ' ')
	b = append(b, formatFloat(v)...)
	return append(b, '\n')
}

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

// Handler serves the metrics over HTTP (GET /metrics).
func (p *Prometheus) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write(p.Write())
	})
}
