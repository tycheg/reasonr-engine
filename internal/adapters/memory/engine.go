// Package memory holds the in-memory adapters of the ReasonR engine (RFC
// §30–31) and the engine for reasonrd test. Store is the main store, an
// asynchronous projection store, a timer source and the outbox; Hub is the
// notification hub; Events is the event store; Clock is a per-tenant
// clock. They are the reference behavior of the ports: the conformance
// tests (engine/conformance) pass here first.
//
// Engine is the kernel (engine/kernel) with these adapters for one model: a
// controlled clock, deterministic identifiers and the delivery journal the
// test driver reads.
package memory

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/features"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Options are the engine settings.
type Options struct {
	// Now is the start time of the engine clock; 2026-01-01T00:00:00Z by default.
	Now time.Time
	// Seed is the high 8 bytes of issued uuids: runs with one Seed give the
	// same identifiers.
	Seed uint64
	// CheckDeterminism: run each command twice and compare the results
	// (reasonrd test --check-determinism).
	CheckDeterminism bool
	// BeforeCommit is called before committing each unit of work without
	// locks: tests call other commands from it to get a version conflict.
	BeforeCommit func(w *reasonr.UnitOfWork)
	// Backend is the engine store instead of the in-memory Store: so the
	// acceptance tests run on other adapters (PostgreSQL). nil — Store.
	Backend func(kind.Catalog) Backend
}

// Backend is the engine store: the main store, timers and the outbox.
type Backend interface {
	kind.UnitStore
	kind.TimerSource
	kind.EffectSource
}

// DefaultNow is the default start time of the clock.
var DefaultNow = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Tenant is the engine's data tenant.
const Tenant = port.DefaultTenant

// Reasons of engine errors.
var (
	ErrConflict      = port.ErrConflict
	ErrNondetermined = kernel.ErrNondetermined
	ErrNoMigrations  = kernel.ErrNoMigrations
)

// Engine is an in-memory engine for one model. Its methods are safe for
// concurrent calls.
type Engine struct {
	k      *kernel.Kernel
	store  Backend
	hub    *Hub
	clock  *Clock
	schema *reasonr.Schema
	flags  *features.Resolver

	mu       sync.Mutex
	log      *reasonr.Arena
	sent     []Delivery
	hubRead  int           // hub journal entries read
	read     port.Position // outbox read
	cascaded port.Position // outbox processed by the fake cascade
	fakeErrs []FakeError   // errors of commands the fakes called
}

// New returns an empty engine of the model of the interpreter interp.
func New(interp reasonr.Interpreter, opts Options) *Engine {
	if opts.Now.IsZero() {
		opts.Now = DefaultNow
	}
	programs := kernel.NewPrograms()
	programs.Set(Tenant, interp)
	e := &Engine{hub: NewHub(true), clock: NewClock(opts.Now), schema: interp.Schema(), log: reasonr.NewArena()}
	if opts.Backend != nil {
		e.store = opts.Backend(programs)
	} else {
		e.store = NewStore(programs)
	}
	var ids atomic.Uint64
	var flagStore port.FeatureStore
	if fs, ok := e.store.(port.FeatureStore); ok {
		flagStore = fs
	}
	e.flags = features.New(flagStore, e.hub, nil)
	k, err := kernel.New(kernel.Config{
		Programs: programs, Store: e.store, Timers: e.store, Hub: e.hub, Clock: e.clock, Features: e.flags,
		UUID:             func() [16]byte { return seeded(opts.Seed, ids.Add(1)) },
		CheckDeterminism: opts.CheckDeterminism, BeforeCommit: opts.BeforeCommit,
	})
	if err != nil {
		panic(err) // Programs and Store are set
	}
	e.k = k
	return e
}

// seeded is a version 4 uuid: seed in the high bytes, the counter n in the low ones.
func seeded(seed, n uint64) [16]byte {
	var u [16]byte
	binary.BigEndian.PutUint64(u[:8], seed)
	binary.BigEndian.PutUint64(u[8:], n)
	u[6] = u[6]&0x0f | 0x40
	u[8] = u[8]&0x3f | 0x80
	return u
}

// Features returns the engine's feature flag rollout.
func (e *Engine) Features() *features.Resolver { return e.flags }

// EnableFeature switches the feature flag name on for the whole system: a
// feature line at the start of a test (RFC §23.4).
func (e *Engine) EnableFeature(_ context.Context, name string) error {
	if e.schema.Feature(name) < 0 {
		return fmt.Errorf("the model has no feature flag %s", name)
	}
	e.flags.SetTest(Tenant, name)
	return nil
}

// Kernel returns the engine kernel.
func (e *Engine) Kernel() *kernel.Kernel { return e.k }

// Store returns the engine store.
func (e *Engine) Store() Backend { return e.store }

// Schema returns the schema of the engine's model.
func (e *Engine) Schema() *reasonr.Schema { return e.schema }

// Now returns the time of the engine clock.
func (e *Engine) Now() time.Time { return e.clock.Now(Tenant) }

// SetNow sets the clock; timers do not fire (Advance does that).
func (e *Engine) SetNow(t time.Time) { e.clock.Set(Tenant, t) }

// Delivery is a notification or an external request "delivered" after
// committing the unit of work with position Position. The values lie in
// the journal arena.
type Delivery struct {
	Position     port.Position
	Notification *reasonr.Notification // nil for an external request
	Request      *reasonr.ExternalRequest
}

// Deliveries returns the journal arena and what was delivered from the
// number from: notifications to the initiator, then to other actors, then
// the external requests of each unit of work. The journal values are valid
// while the engine lives.
func (e *Engine) Deliveries(from int) (*reasonr.Arena, []Delivery) {
	e.mu.Lock()
	defer e.mu.Unlock()
	from = min(from, len(e.sent))
	return e.log, append([]Delivery(nil), e.sent[from:]...)
}

// collect appends to the delivery journal the notifications to the
// initiator from the response, those passed to the hub, and external
// requests from the outbox.
func (e *Engine) collect(res *kind.CommandResult) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if res != nil {
		for _, n := range res.Notifications {
			n := copyNotification(e.log, res.Arena, n)
			e.sent = append(e.sent, Delivery{Position: res.Position, Notification: &n})
		}
	}
	ha, sent := e.hub.Journal(e.hubRead)
	e.hubRead += len(sent)
	for _, s := range sent {
		n := copyNotification(e.log, ha, s.Notification)
		pos := port.Position(0)
		if res != nil {
			pos = res.Position
		}
		e.sent = append(e.sent, Delivery{Position: pos, Notification: &n})
	}
	units, err := e.store.Read(context.Background(), e.read, 0)
	if err != nil {
		return err
	}
	for _, u := range units {
		e.read = u.Position
		for _, rq := range u.Requests {
			e.sent = append(e.sent, Delivery{Position: u.Position, Request: &reasonr.ExternalRequest{
				System: rq.System, Request: rq.Request, Args: e.log.Copy(u.Arena, rq.Args.Value()).Record(),
				Key: e.log.Copy(u.Arena, rq.Key),
			}})
		}
	}
	return nil
}

// Execute runs a command and commits the unit of work; on a version
// conflict the command is repeated (RFC §30.6). The engine sets the call
// time. Errors are in CommandResult.Errors, a uniqueness violation
// included; for an accepted command, notifications without a recipient go
// to the initiator (Caller.Subject). The caller releases the response
// (Release). error is an execution fault.
func (e *Engine) Execute(ctx context.Context, call reasonr.CommandCall) (*kind.CommandResult, error) {
	res, err := e.k.Command(ctx, &kind.CommandRequest{Tenant: Tenant, Call: call})
	if err != nil {
		return nil, err
	}
	if err := e.collect(res); err != nil {
		res.Release()
		return nil, err
	}
	if err := e.cascade(ctx); err != nil {
		res.Release()
		return nil, err
	}
	return res, nil
}

// Query runs a query on the current state. The caller releases the result
// (Release).
func (e *Engine) Query(ctx context.Context, call reasonr.QueryCall) (*reasonr.QueryOutcome, error) {
	return e.k.Query(ctx, &kind.QueryRequest{Tenant: Tenant, Call: call})
}

// Migrate runs the manual migration name: each unit of work is committed
// separately (RFC §32, item 3). Errors of a unit of work stop the migration.
func (e *Engine) Migrate(ctx context.Context, name string) error {
	err := e.k.Migrate(ctx, Tenant, name)
	if cerr := e.collect(nil); err == nil {
		err = cerr
	}
	return err
}

// Advance moves the clock by d (RFC §27.1, time offset ±d): while there is
// a timer with a moment no later than now + d, the clock moves to its
// moment and its command runs as a separate unit of work (CallerTimer).
// Timers scheduled meanwhile within the window fire too; simultaneous ones
// in scheduling order. At the end the clock is now + d. each gets the
// response of each fired command before it is released.
func (e *Engine) Advance(ctx context.Context, d time.Duration, each func(*kind.CommandResult)) error {
	target := e.Now().Add(d)
	if d < 0 { // back (time offset -d): timers do not fire
		e.SetNow(target)
		return nil
	}
	for {
		ts, err := e.store.Lease(ctx, port.LeaseRequest{Tenant: Tenant, Now: target, Limit: 1, TTL: kernel.TimerLease})
		if err != nil {
			return err
		}
		if len(ts) == 0 {
			break
		}
		t := &ts[0]
		if t.At.After(e.Now()) {
			e.SetNow(t.At)
		}
		res, err := e.k.Fire(ctx, t)
		if err != nil {
			return err
		}
		if res == nil {
			continue
		}
		err = e.collect(res)
		if err == nil && each != nil {
			each(res)
		}
		res.Release()
		if err == nil {
			err = e.cascade(ctx)
		}
		if err != nil {
			return err
		}
	}
	e.SetNow(target)
	return nil
}

// Timers returns the number of scheduled timers (the in-memory store).
func (e *Engine) Timers() int {
	s, ok := e.store.(*Store)
	if !ok {
		return -1
	}
	live, _ := s.Timers(Tenant)
	return live
}

// NewID issues an identifier by the type's strategy: uuid — Seed and a
// counter, sequence — a counter per type; t == 0 — a timer identifier.
func (e *Engine) NewID(ctx context.Context, _ *reasonr.Arena, t reasonr.TypeRef) (reasonr.Value, error) {
	return e.k.NewID(ctx, Tenant, t)
}

// Load returns an entity or projection record.
func (e *Engine) Load(ctx context.Context, a *reasonr.Arena, t reasonr.TypeRef, id reasonr.Value) (reasonr.Record, uint64, bool, error) {
	return e.store.Load(ctx, Tenant, a, t, id)
}

// Find runs a search.
func (e *Engine) Find(ctx context.Context, a *reasonr.Arena, q *reasonr.FindSpec) (reasonr.Rows, error) {
	return e.store.Find(ctx, Tenant, a, q)
}

// Events returns an entity's events in order (the in-memory store).
func (e *Engine) Events(a *reasonr.Arena, t reasonr.TypeRef, id reasonr.Value) ([]reasonr.Event, error) {
	s, ok := e.store.(*Store)
	if !ok {
		return nil, errors.New("memory: only the in-memory store has an entity event store")
	}
	return s.Events(Tenant, a, t, id)
}
