package port

import (
	"context"
	"time"
)

// The store ports of adapters (plan 0021, decision 5): a store writes,
// reads and searches rows; everything that needs the model — the state
// encoding, the index key values, personal data encryption and blind keys,
// the outbox, timer and request records — the engine does before it calls
// the store (engine/rows). The data layout of a store is the
// engine's: a row per entity or projection row with its state (canonical
// JSON without personal values), the ciphertexts of its personal values,
// their subjects and the index key columns.

// ColumnKind is how a key column holds its value; the store keeps the
// order of the model (strings bytewise, timestamps by nanoseconds,
// enumerations by number).
type ColumnKind uint8

const (
	ColInt     ColumnKind = iota // int64: int, bigint, date, time, duration, enum, a sequence id
	ColFloat                     // float64
	ColNumeric                   // string: the digits of a decimal
	ColNanos                     // string: a timestamp in Unix nanoseconds (it exceeds int64)
	ColText                      // string: a string, a string id
	ColBytes                     // []byte: bytes
	ColBool                      // bool
	ColUUID                      // [16]byte: uuid, Timer.Id, a uuid id
	ColOpaque                    // []byte: a composite value in the engine's binary encoding — equality only
	ColBlind                     // []byte: the blind value of a personal key (RFC §20) — equality only
)

// Scalar is the value of a key or id column: nil (none), int64, float64,
// string, bool, []byte or [16]byte by its ColumnKind.
type Scalar = any

// Table is the layout of an entity or projection type in a store.
type Table struct {
	Type       string // the model type: Ordering.Order
	Projection bool
	ID         ColumnKind // the id column
	Keys       []Column   // the index key columns
	Indexes    []Index
	// Personal: rows have personal values (the sealed and subjects columns).
	Personal bool
	// Ordered: the order of the key columns and the id is the model's, so
	// the store may sort by them.
	Ordered bool
}

// Column is a key column.
type Column struct {
	Name string // k_customer, k_route__from
	Kind ColumnKind
	// Field is the personal field of a blind key column: forget erases the
	// column when the field's value is erased.
	Field string
}

// Index is an index of key columns ("id" is the id column).
type Index struct {
	Name    string
	Columns []string
	Desc    []bool // for each column: descending

	Unique bool
}

// Layout is a model's storage layout: what the store creates and checks.
type Layout struct {
	Model  string  // the model name: base or an extension
	Tables []Table // every stored type
	// Schema is the model schema (JSON); LastSchema returns it.
	Schema []byte
	// Rewrite lists the tables whose existing rows the engine rewrites
	// (a changed personal marking, a renamed personal field, data written
	// before encryption): EnsureSchema calls Rewriter on each of their rows.
	// The store also rewrites the rows of a table it adds key columns to.
	Rewrite []string
	// Replace lists the key columns ("Type.k_column") that changed between
	// blind and plain: the store drops and adds them again.
	Replace []string
}

// Row is an entity or projection row.
type Row struct {
	ID       Scalar
	Version  uint64
	State    []byte       // canonical JSON without personal values
	Sealed   []byte       // the ciphertexts of personal values: the engine's format
	Subjects []SubjectRef // the subjects of the personal values
	Keys     []Scalar     // the key columns in Table.Keys order
}

// Rewriter rewrites a stored row of table t: forget erases the values of
// forgotten subjects, a schema change fills new key columns and re-encrypts
// personal values. The store calls it in its transaction and writes a
// changed row (changed == true) with the next version.
type Rewriter func(ctx context.Context, tenant Tenant, t *Table, r Row) (out Row, changed bool, err error)

// CondOp is the operator of a search condition.
type CondOp uint8

const (
	OpEq CondOp = iota
	OpNe
	OpLt
	OpLe
	OpGt
	OpGe
	OpIn // Values holds the alternatives
)

// Cond is a condition on a key column ("id" — the id column).
type Cond struct {
	Column string
	Op     CondOp
	Values []Scalar
}

// Order is a sort column ("id" — the id column).
type Order struct {
	Column string
	Desc   bool
}

// Query is a search of rows by key columns.
type Query struct {
	Table *Table
	Conds []Cond
	// Order is the order of rows; empty — any.
	Order []Order
	// After continues after the row with these values of the Order columns
	// (keyset pagination); nil — from the start.
	After []Scalar
	Limit int // < 0 — every row
}

// RowWrite writes a row whole.
type RowWrite struct {
	Table *Table
	Row   Row // Row.Version is the version the row had: 0 — a new row
	// Projection: a synchronous projection row: the store locks the row
	// and writes it with its stored version + 1, without a check.
	Projection bool
	// Key is the canonical JSON of the id: asynchronous projections keep
	// deletion marks by it.
	Key string
}

// RowDelete deletes a projection row.
type RowDelete struct {
	Table *Table
	ID    Scalar
	Key   string // the canonical JSON of the id
}

// TimerRecord is a timer of a unit of work.
type TimerRecord struct {
	ID       [16]byte
	At       time.Time // by the real clock
	Offset   time.Duration
	Entity   string // the entity type
	EntityID []byte // the id's canonical JSON
	Command  string
	Args     []byte // canonical JSON without personal values
	Sealed   []byte
}

// RequestRecord is an external request of a unit of work.
type RequestRecord struct {
	Key             [16]byte // the idempotency key
	System, Request string
	Args, Sealed    []byte
	Timer           *[16]byte // the deadline timer; nil — none
}

// UnitRecord is a unit of work prepared for a store.
type UnitRecord struct {
	Writes  []RowWrite
	Deletes []RowDelete
	// Timers to set; DeleteTimers to delete; Fired is the fired timer
	// deleted in the transaction (ErrTimerGone if it is gone).
	Timers       []TimerRecord
	DeleteTimers [][16]byte
	Fired        *[16]byte
	// Outbox is the effect stream record; nil — no work (a rejected
	// command): no outbox entry and no position.
	Outbox   []byte
	Requests []RequestRecord
	// Forget destroys the keys of the subjects and erases their values
	// through Rewrite in the tenant's rows of ForgetTables (the model's
	// tables with personal values).
	Forget       []SubjectRef
	ForgetTables []*Table
	Rewrite      Rewriter
	// Keys are the subject key versions the unit encrypted with: if one is
	// destroyed by now, the commit fails with ErrConflict.
	Keys []KeyVersion
	// NewKeys are the subject keys the unit created: the store writes them
	// in the transaction (version 1, or the next one over a tombstone of
	// the previous version); a key written by now in another way is
	// ErrConflict.
	NewKeys     []WrappedKey
	Idempotency string // the retry key; Result gives its response until Expires
	// Result encodes the response with the unit's position (0 — no work);
	// nil — no response is stored.
	Result  func(pos Position) ([]byte, error)
	Expires time.Time
	Now     time.Time
}

// UniqueKeyViolation is a unique index violation at commit: the engine
// turns it into the index's business error.
type UniqueKeyViolation struct {
	Type, Index string
}

func (u *UniqueKeyViolation) Error() string {
	return "unique index " + u.Type + "." + u.Index + " violated"
}

// RowReader reads rows.
type RowReader interface {
	Load(ctx context.Context, tenant Tenant, t *Table, id Scalar) (Row, bool, error)
	Find(ctx context.Context, tenant Tenant, q *Query) ([]Row, error)
}

// RowStore is the main transactional row store.
type RowStore interface {
	RowReader
	NextSequence(ctx context.Context, tenant Tenant, typ string, n int) (int64, error)
	// Commit writes a unit of work in one transaction: versions
	// (ErrConflict), the fired timer (ErrTimerGone), uniqueness
	// (*UniqueKeyViolation), the keys, forget, the outbox (its position is
	// returned), the requests and the response by key (ErrDuplicateKey).
	Commit(ctx context.Context, tenant Tenant, u *UnitRecord) (Position, error)
	// Remembered returns the response stored by key, not expired at now; nil — none.
	Remembered(ctx context.Context, tenant Tenant, key string, now time.Time) ([]byte, error)
	Capabilities() Capabilities
}

// LeasedTimerRecord is a leased timer.
type LeasedTimerRecord struct {
	Tenant Tenant
	TimerRecord
	Attempts int
}

// TimerStore holds fired timers with leases.
type TimerStore interface {
	Lease(ctx context.Context, req LeaseRequest) ([]LeasedTimerRecord, error)
	Retry(ctx context.Context, tenant Tenant, id [16]byte, at time.Time) error
	Dead(ctx context.Context, tenant Tenant, id [16]byte, reason string) error
}

// OutboxRecord is a committed unit of work in the effect stream.
type OutboxRecord struct {
	Tenant   Tenant
	Position Position
	Payload  []byte
}

// Outbox gives the committed units of work by position.
type Outbox interface {
	Read(ctx context.Context, from Position, limit int) ([]OutboxRecord, error)
	Wait(ctx context.Context, after Position) error
}

// AsyncProjections is a store of projections written from the effect
// stream. Apply writes the rows of the unit at position pos (version pos)
// unless the row or its deletion mark is newer, then erases the values of
// the subjects forget lists in the rows of the tables forgetTables through
// rewrite; a repeated position changes nothing.
type AsyncProjections interface {
	RowReader
	Apply(ctx context.Context, tenant Tenant, pos Position, writes []RowWrite, deletes []RowDelete,
		forget []SubjectRef, forgetTables []*Table, rewrite Rewriter) error
	Applied(ctx context.Context, tenant Tenant) (Position, error)
	Capabilities() Capabilities
}

// EventRecord is an event in the event store.
type EventRecord struct {
	Tenant   Tenant
	Position Position
	Entity   int // the entity's number in the unit
	Seq      int // the event's number
	Type     string
	ID       []byte // the entity id's canonical JSON
	Version  uint64
	Name     string
	Args     []byte // canonical JSON, personal ones null
	Sealed   []byte
	At       time.Time
}

// EventLog is the event store; it gives the bus its events (EventFeed).
type EventLog interface {
	Append(ctx context.Context, events []EventRecord) error
}

// QueuedRequest is a leased external request.
type QueuedRequest struct {
	Tenant    Tenant
	Position  Position
	At        time.Time
	Request   RequestRecord
	Deadline  time.Time
	TimerGone bool
	Attempts  int
}

// Requests is the queue of external requests in the main store.
type Requests interface {
	LeaseRequests(ctx context.Context, now time.Time, limit int, ttl time.Duration) ([]QueuedRequest, error)
	RequestDone(ctx context.Context, tenant Tenant, key [16]byte) error
	RetryRequest(ctx context.Context, tenant Tenant, key [16]byte, at time.Time, reason string) error
}

// RowSink writes entity rows in batches without locks (kind.EntitySink of
// a kind that keeps its entities in memory): a row is written only if its
// version is greater than the stored one.
type RowSink interface {
	PutRows(ctx context.Context, tenant Tenant, rows []RowWrite) error
}

// TimerExport is the state of a tenant a transfer moves between engines.
type TimerExport struct {
	Timers    []TimerRecord
	Sequences map[string]int64 // by type
}

// Transfer moves a tenant's timers and counters.
type Transfer interface {
	ExportTimers(ctx context.Context, tenant Tenant) (TimerExport, error)
	ImportSequences(ctx context.Context, tenant Tenant, seqs map[string]int64) error
	ClearTimers(ctx context.Context, tenant Tenant) error
}

// Schemas is a store that needs the layout in advance.
type Schemas interface {
	// LastSchema returns the schema of the model written last; nil — never deployed.
	LastSchema(ctx context.Context, model string) ([]byte, error)
	// EnsureSchema creates the missing tables, key columns and indexes, has
	// the rows of Layout.Rewrite rewritten (for the tenants), and records the
	// schema.
	EnsureSchema(ctx context.Context, l *Layout, tenants []Tenant, rewrite Rewriter) error
}
