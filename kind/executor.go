package kind

import (
	"context"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/port"
)

// Executor is the topology boundary: a transport calls it without knowing
// where the command runs — in this process's kernel, in another process
// or in the in-memory engine. The arguments are already decoded by the
// schema, the caller is authenticated.
type Executor interface {
	Catalog // tenant models: for codecs, OpenAPI, Protobuf
	// Command runs a command and commits its unit of work. error is an
	// execution or infrastructure fault (internalError); business errors
	// are in CommandResult.Errors.
	Command(ctx context.Context, req *CommandRequest) (*CommandResult, error)
	// Query runs a query. With After it waits until the asynchronous
	// projections apply the position; if they do not — ErrNotReady.
	Query(ctx context.Context, req *QueryRequest) (*reasonr.QueryOutcome, error)
	// Subscribe subscribes to an actor's notifications from other actors.
	Subscribe(ctx context.Context, tenant port.Tenant, a *reasonr.Arena, actor reasonr.Value) (Subscription, error)
}

// CommandRequest is a command call. The executor sets Call.Now by its own
// clock and Call.Caller.Tenant by Tenant.
type CommandRequest struct {
	Tenant port.Tenant
	Call   reasonr.CommandCall // the arguments are in Call.Arena
	// Idempotency is the retry key (Idempotency-Key): the response is
	// stored with the unit of work, and a retry gets the same response. ""
	// — no key.
	Idempotency string
}

// CommandResult is a command response. The values lie in Arena and are
// valid until Release.
type CommandResult struct {
	// Errors are business errors: the effects are dropped (RFC §30.2).
	Errors []reasonr.BusinessError
	// Notifications go to the command initiator: the transport returns them
	// in the response. Notifications to other actors went through NotificationHub.
	Notifications []reasonr.Notification
	// Position is the position of the committed unit of work; 0 if rejected.
	Position port.Position
	// Replayed: the response is taken by the idempotency key, the command did not run.
	Replayed bool
	// ID is the identifier the engine issued to a create command (RFC
	// §10.3); None for other commands and rejected ones.
	ID    reasonr.Value
	Arena *reasonr.Arena
	// Outcome is the interpreter result that owns Arena (the unit of work —
	// for tests and debugging); nil — Arena is taken from reasonr.GetArena.
	Outcome *reasonr.Outcome
}

// Release frees the response arena. A repeated call does nothing.
func (r *CommandResult) Release() {
	switch {
	case r.Outcome != nil:
		r.Outcome.Release()
	case r.Arena != nil:
		reasonr.PutArena(r.Arena)
	}
	r.Outcome, r.Arena = nil, nil
}

// QueryRequest is a query call.
type QueryRequest struct {
	Tenant port.Tenant
	Call   reasonr.QueryCall
	// After is the position the asynchronous projections must apply before
	// reading; 0 — read at once (RFC §31.3).
	After port.Position
}

// Subscription is a subscription to an actor's notifications.
type Subscription interface {
	// C returns the notification channel; it closes with the subscription.
	C() <-chan Delivery
	Close() error
}

// Delivery is a notification delivered to a subscriber, in its own arena.
type Delivery struct {
	Notification reasonr.Notification
	Arena        *reasonr.Arena
}
