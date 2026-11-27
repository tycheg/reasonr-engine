package port

import (
	"context"
	"time"
)

// The scenario clock offset (RFC §27.5): a test or a demo runs on the
// current time with a shift. The offset belongs to the scenario, not to
// the tenant: the transport takes it from the request (the
// Reasonr-Time-Offset header) and puts it into the context; the kernel
// adds it to the clock of the unit of work.

type offsetKey struct{}

// WithOffset returns a context with the scenario clock offset.
func WithOffset(ctx context.Context, d time.Duration) context.Context {
	if d == 0 {
		return ctx
	}
	return context.WithValue(ctx, offsetKey{}, d)
}

// Offset returns the scenario clock offset from the context; 0 — none.
func Offset(ctx context.Context) time.Duration {
	d, _ := ctx.Value(offsetKey{}).(time.Duration)
	return d
}
