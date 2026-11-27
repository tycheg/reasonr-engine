// Package kernel is the kernel of the ReasonR engine (RFC §30–31), shared
// by the memory and stateless engines. The kernel holds the tenant
// programs, runs commands and queries with the interpreter, routes reads to
// stores (entities and synchronous projections to the main store,
// asynchronous ones to their own), commits units of work, repeating them on
// a version conflict (RFC §30.6), turns uniqueness violations into business
// errors, answers by idempotency keys, runs fired timers and manual
// migrations, and after commit hands notifications to other actors to the hub.
//
// The kernel implements port.Executor and knows nothing of concrete adapters.
package kernel

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Config holds the kernel's components and settings.
type Config struct {
	Programs kind.Programs  // the models of data tenants
	Store    kind.UnitStore // the main store
	// Async holds the stores of asynchronous projections by projection
	// name; other projections are synchronous (in the main store).
	Async  map[string]kind.ProjectionStore
	Timers kind.TimerSource     // nil — timers do not run (Fire is not available)
	Hub    kind.NotificationHub // nil — notifications to other actors are not delivered
	Clock  port.Clock           // nil — the system clock
	// UUID issues uuid bytes (version 4); nil — cryptographically random.
	UUID func() [16]byte
	// Retries is how many times a command runs on version conflicts; 0 —
	// DefaultRetries.
	Retries int
	// ConsistencyWait is how long a query with After waits for asynchronous
	// projections; 0 — DefaultConsistencyWait.
	ConsistencyWait time.Duration
	// IdempotencyTTL is how long a response by key is kept; 0 —
	// DefaultIdempotencyTTL.
	IdempotencyTTL time.Duration
	// TimerAttempts is after how many faults a timer becomes dead; 0 —
	// DefaultTimerAttempts.
	TimerAttempts int
	// Contexts are the bounded contexts the process serves (RFC §23.2):
	// commands and queries of others get port.ErrMisdirected, their ACL
	// reactions do not enter this process's units of work; nil — all.
	Contexts []string
	// CheckDeterminism: run each command twice and compare the results
	// (reasonrd test --check-determinism).
	CheckDeterminism bool
	// BeforeCommit is called before each commit without locks: tests call
	// other commands from it to get a version conflict.
	BeforeCommit func(w *reasonr.UnitOfWork)
	// OnRequests is called after committing a unit of work with external
	// requests: it wakes this process's sender (dispatch.Sender.Wake).
	OnRequests func()
	// Notify gets the notifications of each committed unit of work (the
	// addressed ones too) for the bus without guarantees
	// (dispatch.Notifier.Notify): the values are valid only during the
	// call; nil — not called.
	Notify  func(tenant port.Tenant, pos port.Position, at time.Time, a *reasonr.Arena, ns []reasonr.Notification)
	Logger  *slog.Logger // nil — no logs
	Metrics port.Metrics // nil — no metrics
	// Features is the feature flag rollout (engine/features): the caller's
	// flag set per unit of work; nil — every flag is off.
	Features kind.FeatureSource
}

// features fills the caller's feature flags.
func (k *Kernel) features(tenant port.Tenant, s *reasonr.Schema, c *reasonr.Caller, a *reasonr.Arena) {
	if k.cfg.Features != nil {
		c.Features = k.cfg.Features.Features(tenant, s, c, a)
	}
}

// Features returns the kernel's feature flag rollout or nil (ports.FeatureExecutor).
func (k *Kernel) Features() kind.FeatureSource { return k.cfg.Features }

// Default settings.
const (
	DefaultRetries         = 16
	DefaultConsistencyWait = kind.DefaultConsistencyWait
	DefaultIdempotencyTTL  = 24 * time.Hour
	DefaultTimerAttempts   = 10
	// TimerLease is the lease of a fired timer.
	TimerLease = kind.TimerLease
)

// ErrNondetermined: a repeated execution of a command gave another result.
var ErrNondetermined = errors.New("a repeated execution gave another result")

// Migrator is an interpreter that can run manual migrations: each unit of
// work of a migration is passed to commit (RFC §32, item 3).
type Migrator interface {
	Migrate(ctx context.Context, name string, env reasonr.Env, now time.Time, commit func(*reasonr.Outcome) error) error
}

// ErrNoMigrations: the tenant's interpreter does not run migrations.
var ErrNoMigrations = errors.New("the interpreter does not run migrations")

// Kernel is the engine kernel. Its methods are safe for concurrent calls.
type Kernel struct {
	cfg   Config
	log   *slog.Logger
	clock port.Clock

	mu     sync.Mutex
	placed map[*reasonr.Schema][]kind.ProjectionStore // by TypeRef-1: nil — synchronous

	commands  port.Counter
	duration  port.Histogram
	conflicts port.Counter
	timers    port.Counter
}

// New returns a kernel. Programs and Store are required.
func New(cfg Config) (*Kernel, error) {
	if cfg.Programs == nil || cfg.Store == nil {
		return nil, errors.New("kernel: Programs and Store are required")
	}
	if cfg.Retries <= 0 {
		cfg.Retries = DefaultRetries
	}
	if cfg.ConsistencyWait <= 0 {
		cfg.ConsistencyWait = DefaultConsistencyWait
	}
	if cfg.IdempotencyTTL <= 0 {
		cfg.IdempotencyTTL = DefaultIdempotencyTTL
	}
	if cfg.TimerAttempts <= 0 {
		cfg.TimerAttempts = DefaultTimerAttempts
	}
	if cfg.UUID == nil {
		cfg.UUID = randomUUID
	}
	k := &Kernel{cfg: cfg, log: cfg.Logger, clock: cfg.Clock, placed: map[*reasonr.Schema][]kind.ProjectionStore{}}
	if k.log == nil {
		k.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if k.clock == nil {
		k.clock = ports.SystemClock{}
	}
	m := cfg.Metrics
	if m == nil {
		m = ports.Nop{}
	}
	k.commands = m.Counter("reasonr_commands_total", "Commands by outcome: accepted, rejected, failed.", "outcome")
	k.duration = m.Histogram("reasonr_command_seconds", "Command execution and commit time.", nil)
	k.conflicts = m.Counter("reasonr_conflicts_total", "Command repeats on version conflicts.")
	k.timers = m.Counter("reasonr_timers_total", "Fired timers by outcome: accepted, rejected, failed, dead, forgotten.", "outcome")
	return k, nil
}

func randomUUID() [16]byte {
	var u [16]byte
	_, _ = rand.Read(u[:])
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	return u
}

// Programs holds the models of data tenants: tenant → interpreter. Many
// tenants may use one interpreter (RFC §21.1). It implements port.Catalog.
type Programs struct {
	mu      sync.RWMutex
	tenants map[port.Tenant]reasonr.Interpreter
}

// NewPrograms returns an empty set of programs.
func NewPrograms() *Programs {
	return &Programs{tenants: map[port.Tenant]reasonr.Interpreter{}}
}

// Set assigns a program to a data tenant.
func (p *Programs) Set(t port.Tenant, interp reasonr.Interpreter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tenants[t] = interp
}

// Remove removes a tenant (the ephemeral tenants of tests).
func (p *Programs) Remove(t port.Tenant) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.tenants, t)
}

// Tenants returns the data tenants by name.
func (p *Programs) Tenants() []port.Tenant {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]port.Tenant, 0, len(p.tenants))
	for t := range p.tenants {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Interpreter returns the tenant's program.
func (p *Programs) Interpreter(t port.Tenant) (reasonr.Interpreter, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	interp := p.tenants[t]
	if interp == nil {
		return nil, fmt.Errorf("%w %q", port.ErrUnknownTenant, t)
	}
	return interp, nil
}

// Schema returns the schema of the tenant's model (port.Catalog).
func (p *Programs) Schema(t port.Tenant) (*reasonr.Schema, error) {
	interp, err := p.Interpreter(t)
	if err != nil {
		return nil, err
	}
	return interp.Schema(), nil
}

var _ kind.Executor = (*Kernel)(nil)

// Schema returns the schema of the tenant's model (port.Catalog).
func (k *Kernel) Schema(t port.Tenant) (*reasonr.Schema, error) { return k.cfg.Programs.Schema(t) }

// Clock returns the kernel clock.
func (k *Kernel) Clock() port.Clock { return k.clock }

// Subscribe subscribes to an actor's notifications through the hub.
func (k *Kernel) Subscribe(ctx context.Context, tenant port.Tenant, a *reasonr.Arena, actor reasonr.Value) (kind.Subscription, error) {
	if k.cfg.Hub == nil {
		return nil, errors.New("kernel: no notification hub")
	}
	return k.cfg.Hub.Subscribe(ctx, tenant, a, actor)
}

// Query runs a query. With After it first waits for the tenant's
// asynchronous projections. The caller releases the result (Release).
func (k *Kernel) Query(ctx context.Context, req *kind.QueryRequest) (*reasonr.QueryOutcome, error) {
	interp, err := k.cfg.Programs.Interpreter(req.Tenant)
	if err != nil {
		return nil, err
	}
	if req.After > 0 {
		if err := k.await(ctx, req.Tenant, interp.Schema(), req.After); err != nil {
			return nil, err
		}
	}
	if err := k.local(interp.Schema(), req.Call.Type); err != nil {
		return nil, err
	}
	call := req.Call
	call.Caller.Tenant = string(req.Tenant)
	call.Now = k.clock.Now(req.Tenant).Add(port.Offset(ctx))
	k.features(req.Tenant, interp.Schema(), &call.Caller, call.Arena)
	return interp.Query(ctx, call, &env{k: k, tenant: req.Tenant, schema: interp.Schema()})
}

// await waits until every asynchronous projection store of the model has
// applied the position after; if not within ConsistencyWait — port.ErrNotReady.
func (k *Kernel) await(ctx context.Context, tenant port.Tenant, s *reasonr.Schema, after port.Position) error {
	var stores []kind.ProjectionStore
	for _, ps := range k.placement(s) {
		if ps == nil {
			continue
		}
		dup := false
		for _, x := range stores {
			dup = dup || x == ps
		}
		if !dup {
			stores = append(stores, ps)
		}
	}
	deadline := time.Now().Add(k.cfg.ConsistencyWait)
	for _, ps := range stores {
		for {
			pos, err := ps.Applied(ctx, tenant)
			if err != nil {
				return err
			}
			if pos >= after {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("%w: %d applied, %d needed", port.ErrNotReady, pos, after)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	return nil
}

// placement returns the asynchronous projection stores of the model by
// type number (TypeRef-1); nil — the type is stored in the main store.
func (k *Kernel) placement(s *reasonr.Schema) []kind.ProjectionStore {
	k.mu.Lock()
	defer k.mu.Unlock()
	if p, ok := k.placed[s]; ok {
		return p
	}
	var p []kind.ProjectionStore
	if len(k.cfg.Async) > 0 {
		p = make([]kind.ProjectionStore, len(s.Types))
		for i := range s.Types {
			if s.Types[i].Kind == reasonr.TypeProjection {
				p[i] = k.cfg.Async[s.Types[i].Name]
			}
		}
	}
	k.placed[s] = p
	return p
}

// async returns the store of the asynchronous projection t or nil.
func (k *Kernel) async(s *reasonr.Schema, t reasonr.TypeRef) kind.ProjectionStore {
	p := k.placement(s)
	if t == 0 || int(t) > len(p) {
		return nil
	}
	return p[t-1]
}

// NewID issues an identifier by the type's strategy: uuid — Config.UUID,
// sequence — the main store; t == 0 — a timer identifier.
func (k *Kernel) NewID(ctx context.Context, tenant port.Tenant, t reasonr.TypeRef) (reasonr.Value, error) {
	s, err := k.Schema(tenant)
	if err != nil {
		return reasonr.None, err
	}
	return k.newID(ctx, tenant, s, t)
}

func (k *Kernel) newID(ctx context.Context, tenant port.Tenant, s *reasonr.Schema, t reasonr.TypeRef) (reasonr.Value, error) {
	if t == 0 {
		return reasonr.TimerID(k.cfg.UUID()), nil
	}
	ti := s.Type(t)
	if ti == nil {
		return reasonr.None, fmt.Errorf("kernel: no type %d", t)
	}
	switch ti.ID {
	case reasonr.IDUUID:
		return reasonr.ID(t, reasonr.UUID(k.cfg.UUID())), nil
	case reasonr.IDSequence:
		n, err := k.cfg.Store.NextSequence(ctx, tenant, t, 1)
		if err != nil {
			return reasonr.None, err
		}
		return reasonr.ID(t, reasonr.Bigint(n)), nil
	}
	return reasonr.None, fmt.Errorf("kernel: the caller sets the identifier of %s", ti.Name)
}

// env is the reasonr.Env of one call: it routes reads to stores and issues
// identifiers; ids is the identifier log for the determinism check.
type env struct {
	k      *Kernel
	tenant port.Tenant
	schema *reasonr.Schema
	ids    *idLog
}

func (e *env) reader(t reasonr.TypeRef) kind.Reader {
	if ps := e.k.async(e.schema, t); ps != nil {
		return ps
	}
	return e.k.cfg.Store
}

func (e *env) Load(ctx context.Context, a *reasonr.Arena, t reasonr.TypeRef, id reasonr.Value) (reasonr.Record, uint64, bool, error) {
	return e.reader(t).Load(ctx, e.tenant, a, t, id)
}

func (e *env) Find(ctx context.Context, a *reasonr.Arena, q *reasonr.FindSpec) (reasonr.Rows, error) {
	return e.reader(q.Type).Find(ctx, e.tenant, a, q)
}

func (e *env) NewID(ctx context.Context, _ *reasonr.Arena, t reasonr.TypeRef) (reasonr.Value, error) {
	if e.ids != nil && e.ids.replay {
		return e.ids.next(t)
	}
	v, err := e.k.newID(ctx, e.tenant, e.schema, t)
	if err == nil && e.ids != nil {
		e.ids.add(t, v)
	}
	return v, err
}

// local checks that the type t belongs to a bounded context of this process
// (Config.Contexts); otherwise port.ErrMisdirected. Types outside bounded
// contexts (a flat module) are its own.
func (k *Kernel) local(s *reasonr.Schema, t reasonr.TypeRef) error {
	if k.cfg.Contexts == nil {
		return nil
	}
	ti := s.Type(t)
	if ti == nil {
		return nil
	}
	cx, _, ok := strings.Cut(ti.Name, ".")
	if !ok || slices.Contains(k.cfg.Contexts, cx) {
		return nil
	}
	return fmt.Errorf("%w: %s is in bounded context %s", port.ErrMisdirected, ti.Name, cx)
}
