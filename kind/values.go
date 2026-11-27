package kind

import (
	"context"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/port"
)

// The store interfaces at the level of model values (records in an arena,
// the unit of work, timers, the effect stream): the contract of the
// engine's kernel and of engine kinds. The kernel runs over a UnitStore;
// an engine kind gives the host its effect stream (EffectSource) and may
// keep its state in a store of its own (MemoryStore). A
// store adapter of another repository implements the byte-level ports
// (rows.go); the engine adapts it to these.

// Reader reads entity and projection records (reasonr.Env without NewID).
// Records and cursors are written into the arena a, arguments are read from it.
type Reader interface {
	// Load returns a record; if there is none — default values,
	// exists == false, version == 0.
	Load(ctx context.Context, tenant port.Tenant, a *reasonr.Arena, t reasonr.TypeRef, id reasonr.Value) (state reasonr.Record, version uint64, exists bool, err error)
	// Find runs a search (RFC §30.4): rows by the conditions in sort order
	// (by default — of the index keys), then by identifier; a page — with a
	// cursor after the last row. Limit < 0 without a page — all rows.
	Find(ctx context.Context, tenant port.Tenant, a *reasonr.Arena, q *reasonr.FindSpec) (reasonr.Rows, error)
}

// UnitStore is the main transactional store: entities, synchronous
// projections, timers, the outbox, uniqueness, idempotency keys. The only
// port on the commit path of the stateless engine.
type UnitStore interface {
	Reader
	// NextSequence allocates n sequence numbers of type t and returns the first.
	NextSequence(ctx context.Context, tenant port.Tenant, t reasonr.TypeRef, n int) (first int64, err error)
	// Commit applies a unit of work atomically: it checks versions
	// (ErrConflict), the fired timer (ErrTimerGone), the idempotency key
	// (ErrDuplicateKey) and uniqueness (*UniqueViolation), then writes states
	// and events, synchronous projections, timers, forget, the outbox and
	// the response by key. It returns the position of the unit of work; 0
	// for a unit without Work.
	Commit(ctx context.Context, tenant port.Tenant, u *Unit) (port.Position, error)
	// Remembered returns the response stored by the idempotency key and not
	// expired at now (the engine clock), with the values in the arena a;
	// nil — no response.
	Remembered(ctx context.Context, tenant port.Tenant, key string, now time.Time, a *reasonr.Arena) (*CommandResult, error)
	Capabilities() port.Capabilities
}

// Unit is a unit of work to commit. The values lie in Arena.
type Unit struct {
	Arena *reasonr.Arena
	// Work holds the model's effects; nil — only the timer deletion and the
	// response by key are committed (a rejected command).
	Work *reasonr.UnitOfWork
	// Projections are operations on synchronous projections: applied in the
	// transaction. Async — on asynchronous ones: written only to the outbox.
	Projections []reasonr.ProjectionOp
	Async       []reasonr.ProjectionOp
	// Now is the time of the unit of work by the engine clock.
	Now time.Time
	// FiredTimer is the Timer.Id of the fired timer: its row is deleted in
	// the same transaction; None — the command is not from a timer.
	FiredTimer reasonr.Value
	// Idempotency is the retry key; Result is the response stored by it
	// until Expires.
	Idempotency string
	Result      *CommandResult
	Expires     time.Time
	// Contexts are the bounded contexts of the process that ran the unit:
	// their ACL reactions are already in it (RFC §23.2); nil — all of them.
	Contexts []string
	// Offset is the scenario clock offset (RFC §27.5): Now = the engine
	// clock + Offset. A timer of the unit keeps its moment by the real clock
	// (TimerOp.At − Offset) and its offset. 0 outside tests and demos.
	Offset time.Duration
}

// CommittedUnit is a committed unit of work in the effect stream: what
// asynchronous projections, the event store, buses and the integration
// service get after commit. There are no notifications here: they are not
// stored (RFC §13).
type CommittedUnit struct {
	Tenant      port.Tenant
	Position    port.Position
	At          time.Time
	Arena       *reasonr.Arena
	Entities    []EntityEvents
	Projections []reasonr.ProjectionOp // operations on asynchronous projections
	Requests    []reasonr.ExternalRequest
	Forget      []reasonr.Subject
	// Calls are calls of model commands by fakes (the test program): they
	// run after commit with the role of the fake's system.
	Calls []reasonr.FakeCall
	// Keys are the versions of subject keys that decrypted the unit's
	// personal values: the event store and asynchronous projections encrypt
	// them with these versions (pii.Keyring.Resealer) instead of creating new ones.
	Keys []port.KeyVersion
	// Contexts is Unit.Contexts: the ACL reactions of these bounded contexts
	// already ran in the unit; nil — of all.
	Contexts []string
}

// EntityEvents are the events of one entity in a unit of work.
type EntityEvents struct {
	Type    reasonr.TypeRef
	ID      reasonr.Value
	Version uint64 // the version after commit
	Events  []reasonr.Event
}

// TimerSource holds fired timers with leases. Deleting a timer on success
// is part of Commit (Unit.FiredTimer).
type TimerSource interface {
	// Lease selects timers with a moment no later than req.Now that are not
	// leased and not dead, in the order of the moment and scheduling, and
	// leases them until req.Now + req.TTL.
	Lease(ctx context.Context, req port.LeaseRequest) ([]LeasedTimer, error)
	// Retry releases the lease after a fault: the timer is selected again no earlier than at.
	Retry(ctx context.Context, t *LeasedTimer, at time.Time) error
	// Dead makes a timer dead: it is not selected until an operator command
	// brings it back.
	Dead(ctx context.Context, t *LeasedTimer, reason string) error
}

// LeasedTimer is a leased timer: the command it calls.
type LeasedTimer struct {
	Tenant   port.Tenant
	ID       reasonr.Value // Timer.Id
	At       time.Time     // the moment by the scenario clock (Offset is its offset)
	Entity   reasonr.TypeRef
	EntityID reasonr.Value
	Command  reasonr.MethodRef
	Args     reasonr.Record
	Attempts int // the number of leases, this one included
	Arena    *reasonr.Arena
	// At is the moment by the clock of the scenario that set the timer;
	// Offset is the offset of that clock: the real moment is At − Offset.
	Offset time.Duration
}

// EffectSource holds committed units of work by position (the outbox).
type EffectSource interface {
	// Read returns up to limit units of work with a position greater than
	// from in increasing order; positions do not skip uncommitted ones.
	Read(ctx context.Context, from port.Position, limit int) ([]CommittedUnit, error)
	// Wait waits for a unit of work with a position greater than after.
	Wait(ctx context.Context, after port.Position) error
}

// EntitySink is a store to which an engine kind that keeps its entities in
// memory writes their states after commit: in batches and without locks —
// one writer at a time, and a row is written only if its version is
// greater than the stored one, so a repeated batch is safe.
type EntitySink interface {
	PutEntities(ctx context.Context, tenant port.Tenant, a *reasonr.Arena, rows []EntityState) error
}

// EntityState is the last state of an entity (values in the batch arena).
type EntityState struct {
	Type    reasonr.TypeRef
	ID      reasonr.Value
	State   reasonr.Record
	Version uint64
}

// ProjectionStore is a store of projections written asynchronously, from
// the effect stream: OpenSearch, ClickHouse, a separate PostgreSQL. Queries
// read it through Reader.
type ProjectionStore interface {
	Reader
	// Apply applies the operations of the unit of work at position pos,
	// then erases the data of the subjects it forgot; ops and forget may be
	// empty — then only the position is recorded. Personal values of ops
	// are encrypted with the key versions keys (CommittedUnit.Keys); a value
	// whose key is missing there or already destroyed is written as none. A
	// repeated position not greater than the applied one changes nothing.
	Apply(ctx context.Context, tenant port.Tenant, pos port.Position, a *reasonr.Arena, ops []reasonr.ProjectionOp, keys []port.KeyVersion, forget Forget) error
	// Applied returns the tenant's last applied position.
	Applied(ctx context.Context, tenant port.Tenant) (port.Position, error)
	Capabilities() port.Capabilities
}

// Forget lists the subjects a unit of work forgot, for the asynchronous
// projection store (RFC §20): the store erases their personal values and
// blind values in rows of the projections Types — those written to it
// asynchronously. The main store destroyed the subject keys at commit; rows
// of other types (in the same store — main data with keys of new versions)
// are not touched.
type Forget struct {
	Subjects []reasonr.Subject
	Types    []reasonr.TypeRef
}

// RequestQueue holds external requests waiting to be sent, in the main
// store (RFC §18). The store writes the requests of a unit of work at
// Commit in the same transaction, and the sender (dispatch.Sender) takes
// the ready requests of every tenant and sends them in parallel and
// independently: an unavailable system delays only its own requests.
// Personal arguments are stored as ciphertext.
type RequestQueue interface {
	// LeaseRequests leases the requests of every tenant ready for an attempt
	// no later than now, in the order of the attempt moment and the commit,
	// until now + ttl: another instance does not take them, and if this one
	// fails the lease expires. The attempt counter grows. For a request with
	// a deadline timer, the same store call checks whether the timer is alive.
	LeaseRequests(ctx context.Context, now time.Time, limit int, ttl time.Duration) ([]LeasedRequest, error)
	// RequestDone deletes a request: it is delivered or need not be sent
	// anymore.
	RequestDone(ctx context.Context, r *LeasedRequest) error
	// RetryRequest schedules the next attempt no earlier than at; reason is
	// the failure reason for the operator.
	RetryRequest(ctx context.Context, r *LeasedRequest, at time.Time, reason string) error
}

// Moving state between engines (RFC §32): entities and
// projections move as rows, and timers and sequence counters through
// StateTransfer.

// TimerState is a live timer of a tenant.
type TimerState struct {
	ID       reasonr.Value
	At       time.Time     // the moment by the real clock
	Offset   time.Duration // the scenario clock offset (RFC §27.5)
	Entity   reasonr.TypeRef
	EntityID reasonr.Value
	Command  reasonr.MethodRef
	// Args are the command arguments, personal ones as plaintext; Subjects
	// are their subjects: the receiving store encrypts with them.
	Args     reasonr.Record
	Subjects []reasonr.ArgSubject
}

// EngineState holds the timers and sequence counters of a tenant (the last
// issued number by type).
type EngineState struct {
	Timers    []TimerState
	Sequences map[reasonr.TypeRef]int64
}

// StateTransfer is a store from which a tenant's state is exported and
// into which it is imported. Timers are imported with a unit of work
// (UnitStore.Commit with Work.Timers): so their arguments are encrypted as usual.
type StateTransfer interface {
	// ExportState returns the live timers and counters of a tenant; the
	// values are in the arena a.
	ExportState(ctx context.Context, tenant port.Tenant, a *reasonr.Arena) (EngineState, error)
	// ImportSequences raises the tenant counters to seqs (never lowers them).
	ImportSequences(ctx context.Context, tenant port.Tenant, seqs map[reasonr.TypeRef]int64) error
	// ClearTimers deletes a tenant's timers: a transfer moves them,
	// otherwise a timer would fire in the source too.
	ClearTimers(ctx context.Context, tenant port.Tenant) error
}
