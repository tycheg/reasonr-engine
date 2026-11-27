package kind

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/config"
	"github.com/tycheg/reasonr-engine/port"
)

// Engine kinds (plan 0021, decision 7): an engine implementation chosen by
// `engine:` in engine.yaml — stateless (reasonr-stateless) or a kind of
// another module. The host (the engine) builds the stores, the
// ingress, authentication, notifications, the test API, the effect stream
// and telemetry around any kind; the kind gives the executor and its
// background roles. It gets the shared part as interfaces from Host.

// Config is the part of engine.yaml an engine kind reads.
type Config struct {
	// Engine is the kind (engine:); Settings is its section, named after
	// the kind.
	Engine   string
	Settings *port.Node
	// Stores are the stores by name; State is the main store.
	Stores map[string]*port.Node
	State  string
	// Bus is the bus section; nil — no bus.
	Bus *port.Node
	// Contexts are the bounded contexts this process serves; nil — all.
	Contexts []string
	// Tenants are the permanent data tenants by name: the model (base or
	// an extension).
	Tenants map[string]string
	// Limits are the kernel limits.
	Limits Limits
	// Projections are the stores of the projections (projections:): by the
	// full or short name, "default" for the rest.
	Projections map[string]string
}

// Limits are the kernel limits (limits: in engine.yaml).
type Limits struct {
	Retries         int
	ConsistencyWait time.Duration
	IdempotencyTTL  time.Duration
}

// Projection returns the store of the projection name (the model's full
// name or the short one); explicit — it is named in projections:, not the
// default.
func (c *Config) Projection(name string) (store, key string, explicit bool) {
	short := name[strings.LastIndex(name, ".")+1:]
	for _, k := range []string{name, short} {
		if s, ok := c.Projections[k]; ok {
			return s, k, true
		}
	}
	return c.Projections["default"], "default", false
}

// configOf is the view of a loaded engine.yaml.
func configOf(c *config.Config) *Config {
	return &Config{Engine: c.Engine, Settings: c.Settings, Stores: c.Stores, State: c.State, Bus: c.Bus, Contexts: c.Contexts,
		Tenants: c.Tenants, Limits: Limits(c.Limits), Projections: c.Projections}
}

// LoadConfig reads engine.yaml; DecodeConfig decodes its text.
func LoadConfig(path string) (*Config, error) {
	c, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	return configOf(c), nil
}

func DecodeConfig(data []byte) (*Config, error) {
	c, err := config.Decode(data)
	if err != nil {
		return nil, err
	}
	return configOf(c), nil
}

// StoredType reports whether a type is stored as rows: aggregates and
// actors (inner entities live in the document of their aggregate).
func StoredType(ti *reasonr.TypeInfo) bool {
	return (ti.Kind == reasonr.TypeEntity || ti.Kind == reasonr.TypeActor) && ti.Owner == 0
}

// Kind is an engine kind; it registers itself with registry.RegisterKind.
type Kind interface {
	// Open opens the engine of this kind on the host h.
	Open(ctx context.Context, h Host) (Engine, error)
}

// KindTool is a kind whose tool processes (reasonrd migrate, the kind's
// own commands) need the configuration adjusted: for example, to reach the
// running engine instead of holding state of their own. An error means the
// configuration does not say how.
type KindTool interface {
	Tool(cfg *Config) error
}

// KindTestRoles is a kind whose tests need host roles next to it (the
// stateless timers and dispatch); without it the kind runs them itself.
type KindTestRoles interface {
	TestRoles() []string
}

// Engine is an open engine of a kind.
type Engine interface {
	// Executor runs commands and queries; nil — the process serves no API
	// (a process that only keeps state for others).
	Executor() Executor
	// Roles are the kind's background work; the host runs those named in
	// --roles (and the unnamed ones always). A kind runs its effect stream
	// with Host.Effects in a role of its own.
	Roles() []Role
	Close() error
}

// Role is background work of an engine kind.
type Role struct {
	// Name is the role in --roles (timers); "" — it always runs.
	Name string
	Run  func(ctx context.Context) error
}

// Host is the shared part of the engine a kind gets.
type Host interface {
	// Config is engine.yaml; Settings is the kind's section in it.
	Config() *Config
	Settings() *port.Node
	// Test is the engine's test mode (reasonrd --test).
	Test() bool
	Log() *slog.Logger
	Metrics() port.Metrics
	// Models are the compiled models by name (base or an extension);
	// Programs are the models of the data tenants.
	Models() map[string]reasonr.Interpreter
	Programs() Programs
	// Store is a configured store as model values (stores.<name>); State is
	// the main store, Timers its timers (nil — it keeps none).
	Store(name string) any
	State() UnitStore
	Timers() TimerSource
	Hub() NotificationHub
	// KernelConfig is the configuration of a kernel over the main store
	// (its timers, the asynchronous projections, the hub, the clock, the
	// limits); a kind builds its kernels with NewKernel from it.
	KernelConfig() KernelConfig
	NewKernel(cfg KernelConfig) (Kernel, error)
	NewPrograms() Programs
	// TimerTenants are the tenants whose fired timers the kind runs: all
	// (all is true), or in test mode the ordinary ones — an ephemeral
	// tenant lives by a virtual clock and its timers fire through
	// /_test/{tenant}/clock.
	TimerTenants() (tenants []port.Tenant, all bool)
	// Coordinator shares the leases and cursors of the main store's effect
	// stream between the instances (a store of one process: in memory).
	Coordinator() port.Coordinator
	// OnRequests adds f to what the host's kernel calls after a unit of
	// work with external requests (to wake the request sender).
	OnRequests(f func())
	// NewMemoryStore is a store in process memory (the state of a kind that
	// keeps it in memory): opts.KMS wraps the tenant keys.
	NewMemoryStore(catalog Catalog, opts MemoryOptions) MemoryStore
	// NewCoordinator is a coordinator in process memory.
	NewCoordinator() port.Coordinator
	// NewFeatures is the feature flag rollout over a store (nil — every
	// flag is off but the test ones).
	NewFeatures(store port.FeatureStore) FeatureResolver
	// LocalKMS is a KMS with the master key key; rand makes its nonces
	// deterministic (nil — random).
	LocalKMS(key []byte, rand io.Reader) (port.KMS, error)
	// Effects builds the consumers of an effect stream of a kind: the
	// projections, the event store, the forget journal, the requests.
	Effects(cfg EffectsConfig) (*Effects, error)
	// ACL is a consumer that runs the ACL reactions of the bounded
	// contexts in other processes (RFC §23.2) with run.
	ACL(contexts []string, catalog Catalog, run DeliverFunc) Consumer
	// ExportEntities reads the entities of a tenant in batches; ImportState
	// writes timers and counters (moving state between engines, RFC §32).
	ExportEntities(ctx context.Context, src Reader, s *reasonr.Schema, tenant port.Tenant, each func(a *reasonr.Arena, rows []EntityState) error) error
	ImportState(ctx context.Context, dst UnitStore, tenant port.Tenant, a *reasonr.Arena, st EngineState, now time.Time) error
	// SetTestEngine replaces the engine the test API runs on (the kind's
	// executor instead of the host's kernel).
	SetTestEngine(x TestEngine)
	// AddCloser closes c with the engine.
	AddCloser(c port.Closer)
}

// Maintainer is an executor that runs the data operations of reasonrd:
// a data migration of a tenant (reasonrd migrate --run) and forgetting
// subjects again (reasonrd pii replay). A kernel is; an executor of
// another kind may be.
type Maintainer interface {
	Migrate(ctx context.Context, tenant port.Tenant, name string) error
	Forget(ctx context.Context, tenant port.Tenant, subjects []port.SubjectRef) (port.Position, error)
}

// Programs holds the models of data tenants: tenant → interpreter.
type Programs interface {
	Catalog
	Set(t port.Tenant, interp reasonr.Interpreter)
	Remove(t port.Tenant)
	Tenants() []port.Tenant
	Interpreter(t port.Tenant) (reasonr.Interpreter, error)
}

// Timer settings of the kernel.
const (
	// TimerLease is how long a fired timer is leased to an instance.
	TimerLease = 30 * time.Second
	// DefaultConsistencyWait is how long a query with After waits for the
	// asynchronous projections by default.
	DefaultConsistencyWait = 2 * time.Second
)

// KernelConfig is the configuration of a kernel: the executor of calls
// over one main store.
type KernelConfig struct {
	Programs Programs
	Store    UnitStore
	// Async holds the stores of asynchronous projections by projection name.
	Async  map[string]ProjectionStore
	Timers TimerSource     // nil — timers do not run (Fire is not available)
	Hub    NotificationHub // nil — notifications to other actors are not delivered
	Clock  port.Clock      // nil — the system clock
	// UUID issues uuid bytes (version 4); nil — random.
	UUID            func() [16]byte
	Retries         int
	ConsistencyWait time.Duration
	IdempotencyTTL  time.Duration
	// Contexts are the bounded contexts the process serves; nil — all.
	Contexts []string
	// OnRequests is called after a unit with external requests.
	OnRequests func()
	// Notify gets the notifications of each committed unit (the bus).
	Notify   func(tenant port.Tenant, pos port.Position, at time.Time, a *reasonr.Arena, ns []reasonr.Notification)
	Logger   *slog.Logger
	Metrics  port.Metrics
	Features FeatureSource
}

// Kernel executes calls over one main store.
type Kernel interface {
	Executor
	Clock() port.Clock
	NewID(ctx context.Context, tenant port.Tenant, t reasonr.TypeRef) (reasonr.Value, error)
	// Fire runs the command of a fired timer.
	Fire(ctx context.Context, t *LeasedTimer) (*CommandResult, error)
	// Deliver runs the ACL reactions to an event another process committed.
	Deliver(ctx context.Context, tenant port.Tenant, a *reasonr.Arena, t reasonr.TypeRef, id reasonr.Value, ev reasonr.MethodRef, args reasonr.Record, key string) (*CommandResult, error)
	Migrate(ctx context.Context, tenant port.Tenant, name string) error
	Forget(ctx context.Context, tenant port.Tenant, subjects []port.SubjectRef) (port.Position, error)
}

// DeliverFunc runs the ACL reactions to an event (Kernel.Deliver).
type DeliverFunc func(ctx context.Context, tenant port.Tenant, a *reasonr.Arena, t reasonr.TypeRef, id reasonr.Value, ev reasonr.MethodRef, args reasonr.Record, key string) (*CommandResult, error)

// MemoryOptions are the settings of a store in memory.
type MemoryOptions struct {
	KMS port.KMS
	// Subjects is the size of the subject key cache: < 0 — every key.
	Subjects int
	// Now and Rand make the keys deterministic (copies of one state).
	Now  func() time.Time
	Rand io.Reader
}

// MemoryStore is a store in process memory: the main store, projections,
// timers, the outbox, the request queue, the state transfer, and the
// working set of a kind that keeps its state in memory.
type MemoryStore interface {
	UnitStore
	ProjectionStore
	TimerSource
	EffectSource
	RequestQueue
	StateTransfer
	EntitySink
	port.FeatureStore
	// Position is the last committed position; StartPositions continues the
	// positions after after (a restored node); Trim drops the outbox up to upto.
	Position() port.Position
	StartPositions(after port.Position)
	Trim(upto port.Position)
	// Snapshot and Restore write and read the whole state.
	Snapshot(w io.Writer) error
	Restore(r io.Reader) error
	// Digest is the hash of the state (comparing copies of one state).
	Digest() uint64
	// Due reports whether a timer of a tenant not skipped has fired by now.
	Due(now time.Time, skip func(port.Tenant) bool) bool
	// RowKey, Has, UniqueKey and Evict serve the working set: the key of a
	// row, its presence, a unique index key and its owner, the eviction of
	// rows evict chooses.
	RowKey(tenant port.Tenant, a *reasonr.Arena, t reasonr.TypeRef, id reasonr.Value) (string, error)
	Has(tenant port.Tenant, a *reasonr.Arena, t reasonr.TypeRef, id reasonr.Value) (bool, error)
	UniqueKey(tenant port.Tenant, a *reasonr.Arena, t reasonr.TypeRef, in *reasonr.IndexInfo, state, id reasonr.Value) (key string, taken, ok bool, err error)
	Evict(ctx context.Context, tenant port.Tenant, evict func(t reasonr.TypeRef, key string, last port.Position) bool) map[reasonr.TypeRef][]string
	DropTenant(ctx context.Context, t port.Tenant) error
}

// FeatureResolver is the feature flag rollout: the flags of a caller,
// the test switches.
type FeatureResolver interface {
	FeatureSource
	Load(ctx context.Context) error
	SetTest(tenant port.Tenant, name string)
	DropTest(tenant port.Tenant)
}

// Consumer handles the effect stream: the cursor, the lease and the dead
// letters go by its name.
type Consumer interface {
	Name() string
	// Deliver delivers the effects of a unit; only >= 0 — only that effect.
	Deliver(ctx context.Context, u *CommittedUnit, only int, bury func(item int, reason string) error) error
}

// EffectsConfig is an effect stream of a kind and what consumes it.
type EffectsConfig struct {
	Source  EffectSource
	Catalog Catalog
	// Projections are the projection names by store this stream writes;
	// nil — every asynchronous projection of the configuration.
	Projections map[string]map[string]bool
	// Consumers are the kind's own (ACL forwarding between its parts).
	Consumers []Consumer
	// Requests is the queue of external requests the stream sends; nil — none.
	Requests RequestQueue
	Logger   *slog.Logger
}

// Effects are the consumers of a stream.
type Effects struct {
	// Names are the consumer names: the kind keeps their cursors.
	Names []string
	// Run runs the consumers with the cursors of coord, and the request
	// sender; Wake wakes the sender after a unit with requests (nil — no sender).
	Run  func(coord port.Coordinator) func(ctx context.Context) error
	Wake func()
	// Retry delivers the dead letters of the stream with the cursors of
	// coord again (after the recipient is fixed): consumer "" — of every
	// consumer; the delivered ones are deleted.
	Retry func(coord port.Coordinator) func(ctx context.Context, consumer string) (int, error)
}

// TestEngine is what the test API of test mode runs on (/_test): the
// host's kernel or the kind's executor.
type TestEngine interface {
	Command(ctx context.Context, req *CommandRequest) (*CommandResult, error)
	NewID(ctx context.Context, tenant port.Tenant, t reasonr.TypeRef) (reasonr.Value, error)
	// Load reads an entity record: whether a fake is created.
	Load(ctx context.Context, tenant port.Tenant, a *reasonr.Arena, t reasonr.TypeRef, id reasonr.Value) (reasonr.Record, uint64, bool, error)
	// Read gives the outbox units of work after from (external requests and fake calls).
	Read(ctx context.Context, from port.Position, limit int) ([]CommittedUnit, error)
	CreateTenant(ctx context.Context, t port.Tenant, model string, now time.Time) error
	DropTenant(ctx context.Context, t port.Tenant) error
	SetTestFeature(ctx context.Context, t port.Tenant, name string) error
	// Advance moves the tenant's clock by d (for a scenario clock — from the
	// moment with the offset offset): fired timers run in order, each gets
	// the response of each.
	Advance(ctx context.Context, t port.Tenant, d, offset time.Duration, scenario bool, each func(*CommandResult) error) error
}
