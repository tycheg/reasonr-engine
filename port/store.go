package port

import (
	"context"
	"time"
)

// LeaseRequest selects fired timers.
type LeaseRequest struct {
	Tenant Tenant // "" — every tenant
	Now    time.Time
	Limit  int
	TTL    time.Duration
	// Scenario: Now is the time by the scenario clock: a timer fires if its
	// moment with its own offset (LeasedTimer.At) is no later than Now (the
	// time offset shift in a test or a demo, RFC §27.5). Otherwise Now is
	// the real time, and a timer goes by its real moment.
	Scenario bool
}

// Capabilities are a store's capabilities: reasonrd check matches them
// against the model's searches and projections (RFC §31.1).
type Capabilities struct {
	Transactions bool    // atomic commit of a unit of work
	Unique       bool    // unique indexes
	Conds        CondSet // kinds of search conditions
	SortDesc     bool    // desc keys
	Pages        bool    // pages with a cursor
	Scan         bool    // a search without an index (all in migrations)
	FullText     bool
	Personal     bool // personal data: ciphertexts, blind indexes, forget (RFC §20)
	// OpaqueBytes: ColBytes columns compare for equality only (their order
	// in the store is not bytewise): a search ordered by them runs in the
	// engine.
	OpaqueBytes bool
}

// CondSet is a set of search condition kinds.
type CondSet uint16

// AllConds holds every condition kind.
const AllConds = CondSet(1<<(OpIn+1) - 1)

// Has reports whether the condition kind is in the set.
func (s CondSet) Has(op CondOp) bool { return s&(1<<op) != 0 }

// Coordinator is the effect stream state in the main store: consumer
// cursors, leases (one instance handles a tenant's stream) and dead letters.
type Coordinator interface {
	// Cursor returns the position up to which the consumer has processed the stream.
	Cursor(ctx context.Context, consumer string) (Position, error)
	// SetCursor remembers the processed position.
	SetCursor(ctx context.Context, consumer string, pos Position) error
	// Acquire takes or extends the lease name for holder until now + ttl;
	// false — another holds the lease.
	Acquire(ctx context.Context, name, holder string, now time.Time, ttl time.Duration) (bool, error)
	// Bury writes a dead letter: the effect item of the unit of work pos
	// was not delivered to the consumer, and a retry will not help.
	Bury(ctx context.Context, d DeadLetter) error
	// DeadLetters returns the dead letters of a consumer ("" — of all).
	DeadLetters(ctx context.Context, consumer string) ([]DeadLetter, error)
	// Resurrect deletes a dead letter (after a redelivery).
	Resurrect(ctx context.Context, d DeadLetter) error
}

// DeadLetter is an undelivered effect.
type DeadLetter struct {
	Consumer string
	Position Position
	Item     int // the effect number in the unit of work (an external request); -1 — the whole unit
	Tenant   Tenant
	Reason   string
	At       time.Time
}
