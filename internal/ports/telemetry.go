package ports

import (
	"context"
	"time"

	"github.com/tycheg/reasonr-engine/port"
)

// Tracer makes spans "ingress → execution → commit → effects".
type Tracer interface {
	Start(ctx context.Context, name string) (context.Context, Span)
}

// Span is a part of a trace.
type Span interface {
	SetAttr(key, value string)
	End(err error)
}

// SystemClock is the real clock in UTC.
type SystemClock struct{}

func (SystemClock) Now(port.Tenant) time.Time { return time.Now().UTC() }

type Checker interface {
	Health(ctx context.Context) error
}

// Nop is metrics and tracing that do nothing.
type Nop struct{}

func (Nop) Counter(string, string, ...string) port.Counter { return Nop{} }

func (Nop) Histogram(string, string, []float64, ...string) port.Histogram { return Nop{} }

func (Nop) Gauge(string, string, ...string) port.Gauge { return Nop{} }

func (Nop) Add(float64, ...string) {}

func (Nop) Observe(float64, ...string) {}

func (Nop) Set(float64, ...string) {}

func (Nop) Start(ctx context.Context, _ string) (context.Context, Span) {
	return ctx, Nop{}
}

func (Nop) SetAttr(string, string) {}

func (Nop) End(error) {}

var (
	_ port.Metrics = Nop{}
	_ Tracer       = Nop{}
)
