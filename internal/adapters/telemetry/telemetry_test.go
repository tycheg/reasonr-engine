package telemetry

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrometheus(t *testing.T) {
	p := NewPrometheus()
	c := p.Counter("reasonr_commands_total", "Commands.", "outcome")
	c.Add(2, "accepted")
	c.Add(1, `re"j\ected`)
	c.Add(-1, "accepted")
	if again := p.Counter("reasonr_commands_total", "another"); again == nil {
		t.Fatal("registering again")
	}
	h := p.Histogram("reasonr_command_seconds", "Time.", []float64{0.1, 1})
	h.Observe(0.05)
	h.Observe(0.5)
	p.Gauge("reasonr_lag", "Lag.").Set(3)
	want := `# HELP reasonr_command_seconds Time.
# TYPE reasonr_command_seconds histogram
reasonr_command_seconds_bucket{le="0.1"} 1
reasonr_command_seconds_bucket{le="1"} 2
reasonr_command_seconds_bucket{le="+Inf"} 2
reasonr_command_seconds_sum 0.55
reasonr_command_seconds_count 2
# HELP reasonr_commands_total Commands.
# TYPE reasonr_commands_total counter
reasonr_commands_total{outcome="accepted"} 2
reasonr_commands_total{outcome="re\"j\\ected"} 1
# HELP reasonr_lag Lag.
# TYPE reasonr_lag gauge
reasonr_lag 3
`
	if got := string(p.Write()); got != want {
		t.Errorf("output:\n%s\nwant:\n%s", got, want)
	}
}

func TestLogger(t *testing.T) {
	var b bytes.Buffer
	l, err := Logger(&b, "json", "warn")
	if err != nil {
		t.Fatal(err)
	}
	l.Info("not visible")
	l.Warn("visible", "tenant", "t1")
	if s := b.String(); strings.Contains(s, "not visible") || !strings.Contains(s, `"tenant":"t1"`) {
		t.Errorf("log: %s", s)
	}
	for _, c := range [][2]string{{"xml", "info"}, {"text", "loud"}} {
		if _, err := Logger(&b, c[0], c[1]); err == nil {
			t.Errorf("%v: no error", c)
		}
	}
}
