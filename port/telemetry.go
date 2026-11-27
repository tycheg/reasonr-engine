package port

import (
	"context"
	"time"
)

// Cross-cutting ports: metrics, clocks, the lifecycle. Logs are
// the standard *slog.Logger, the format and output are an slog.Handler.

// Metrics is a factory of labeled metrics: Prometheus pull, OTLP push.
type Metrics interface {
	Counter(name, help string, labels ...string) Counter
	Histogram(name, help string, buckets []float64, labels ...string) Histogram
	Gauge(name, help string, labels ...string) Gauge
}

// Counter is an increasing counter; values are the label values in order.
type Counter interface {
	Add(delta float64, values ...string)
}

// Histogram is a distribution of values.
type Histogram interface {
	Observe(v float64, values ...string)
}

// Gauge is a current value.
type Gauge interface {
	Set(v float64, values ...string)
}

// Clock is the engine clock: real, or virtual per tenant (test mode).
type Clock interface {
	Now(t Tenant) time.Time
}

// Components may implement the lifecycle interfaces: the server starts
// them and closes them on shutdown.
type (
	Starter interface {
		Start(ctx context.Context) error
	}
	Closer interface{ Close() error }
)
