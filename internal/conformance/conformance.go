// Package conformance holds the conformance tests of the engine ports (RFC
// §30–31): the same set of checks runs on the memory adapters and on every
// other adapter (PostgreSQL, Redis…). Memory is the reference behavior.
//
// An adapter wires the set into its own test:
//
//	func TestConformance(t *testing.T) {
//		conformance.RunUnitStore(t, func(t *testing.T, c port.Catalog) conformance.Stores {
//			s := mystore.New(c)
//			return conformance.Stores{Unit: s, Timers: s, Effects: s}
//		})
//	}
//
// The kernel (engine/kernel) builds the units of work for the checks with
// the interpreter by the model testdata/store: so the store gets them in
// the same form as in production.
package conformance

import (
	"context"
	_ "embed"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

//go:embed testdata/store/store.rr
var storeModel []byte

//go:embed testdata/pii/pii.rr
var piiModel []byte

// model is the checks model, built once.
type model struct {
	name    string
	src     []byte
	once    sync.Once
	machine *reasonr.Model
	err     error
}

var (
	stores   = &model{name: "store.rr", src: storeModel}
	personal = &model{name: "pii.rr", src: piiModel}
)

func (m *model) get(t testing.TB) *reasonr.Model {
	t.Helper()
	m.once.Do(func() { m.machine, m.err = build(m.name, m.src) })
	if m.err != nil {
		t.Fatal(m.err)
	}
	return m.machine
}

// Machine returns the interpreter of the conformance test model (testdata/store).
func Machine(t testing.TB) *reasonr.Model { return stores.get(t) }

// PersonalMachine returns the machine of the personal data checks model
// (testdata/pii).
func PersonalMachine(t testing.TB) *reasonr.Model { return personal.get(t) }

func build(name string, src []byte) (*reasonr.Model, error) {
	dir, err := os.MkdirTemp("", "reasonr-conformance")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, name), src, 0o644); err != nil {
		return nil, err
	}
	return reasonr.Build(dir, reasonr.Options{Test: true})
}

// The data tenants of the checks: both have the model testdata/store.
const (
	T1 port.Tenant = "t1"
	T2 port.Tenant = "t2"
)

// Start is the start time of the checks clock.
var Start = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Stores are the commit path adapters under test.
type Stores struct {
	Unit    kind.UnitStore
	Timers  kind.TimerSource
	Effects kind.EffectSource
	// Dump returns everything the store keeps, as is (rows, events, the
	// outbox, timers, responses by key): RunPersonal looks for plaintext
	// personal data in it. nil — the check is skipped.
	Dump func() []byte
	// Twin is a second instance of the store over the same data with its
	// own key cache (another engine process): RunPersonal checks writing
	// with a stale key on it. nil — the check is skipped.
	Twin kind.UnitStore
	// Separate is an empty store of the same kind in another database (the
	// event store and asynchronous projections apart from the main one):
	// RunPersonal checks that it encrypts with the main store's keys (RFC
	// §31.2). nil — the check is skipped.
	Separate any
	// KMS is the master key the store was created with: RunPersonal checks
	// rotation on it. nil — the check is skipped.
	KMS port.KMS
}

// Factory creates empty stores for a catalog; each check gets new ones.
type Factory func(t *testing.T, catalog kind.Catalog) Stores

// clock is the checks clock, set by hand.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now(port.Tenant) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}

// fixture is a kernel with the stores under test.
type fixture struct {
	t      *testing.T
	m      *reasonr.Model
	s      *reasonr.Schema
	st     Stores
	k      *kernel.Kernel
	p      *kernel.Programs
	clock  *clock
	a      *reasonr.Arena // the call arena
	tenant port.Tenant
	hook   func(*reasonr.UnitOfWork)
}

// options are the settings of the check's kernel.
type options struct {
	async   map[string]kind.ProjectionStore
	hub     kind.NotificationHub
	machine *reasonr.Model // nil — the model testdata/store
}

func newFixture(t *testing.T, f Factory, o options) *fixture {
	t.Helper()
	m := o.machine
	if m == nil {
		m = Machine(t)
	}
	programs := kernel.NewPrograms()
	programs.Set(T1, m)
	programs.Set(T2, m)
	fx := &fixture{t: t, m: m, s: m.Schema(), st: f(t, programs), p: programs, clock: &clock{now: Start}, a: reasonr.NewArena(), tenant: T1}
	var ids uint64
	var mu sync.Mutex
	k, err := kernel.New(kernel.Config{
		Programs: programs, Store: fx.st.Unit, Timers: fx.st.Timers, Hub: o.hub, Clock: fx.clock, Async: o.async,
		UUID: func() [16]byte {
			mu.Lock()
			defer mu.Unlock()
			ids++
			return [16]byte{6: 0x40, 8: 0x80, 14: byte(ids >> 8), 15: byte(ids)}
		},
		BeforeCommit: func(w *reasonr.UnitOfWork) {
			if fx.hook != nil {
				fx.hook(w)
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	fx.k = k
	return fx
}

func (f *fixture) typ(name string) reasonr.TypeRef {
	f.t.Helper()
	ti := f.s.Lookup(name)
	if ti == nil {
		f.t.Fatalf("no type %s", name)
	}
	return ti.Ref
}

func (f *fixture) method(t reasonr.TypeRef, name string) reasonr.MethodRef {
	f.t.Helper()
	mi := f.s.Type(t).Method(name)
	if mi == nil {
		f.t.Fatalf("no method %s", name)
	}
	return mi.Ref
}

func (f *fixture) fieldRef(t, name string) reasonr.FieldRef {
	f.t.Helper()
	fi := f.s.Type(f.typ(t)).Field(name)
	if fi == nil {
		f.t.Fatalf("no field %s.%s", t, name)
	}
	return fi.Ref
}

func (f *fixture) id(t string) reasonr.Value {
	f.t.Helper()
	v, err := f.k.NewID(context.Background(), f.tenant, f.typ(t))
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

var admin = reasonr.Caller{Kind: reasonr.CallerUser, Roles: []string{"admin"}}

func actor(id reasonr.Value) reasonr.Caller {
	return reasonr.Caller{Kind: reasonr.CallerUser, Subject: id, Actor: id.Type}
}

// command runs a command with the idempotency key key; a fault fails the
// test. The caller releases the response.
func (f *fixture) command(key string, c reasonr.Caller, id reasonr.Value, cmd string, args ...reasonr.Value) *kind.CommandResult {
	f.t.Helper()
	res, err := f.k.Command(context.Background(), &kind.CommandRequest{Tenant: f.tenant, Idempotency: key, Call: reasonr.CommandCall{
		Entity: id.Type, ID: id, Command: f.method(id.Type, cmd), Args: f.a.Record(0, args...), Arena: f.a, Caller: c,
	}})
	if err != nil {
		f.t.Fatalf("%s: %v", cmd, err)
	}
	return res
}

// exec runs a command and returns the errors with arguments; a fault fails the test.
func (f *fixture) exec(c reasonr.Caller, id reasonr.Value, cmd string, args ...reasonr.Value) []string {
	f.t.Helper()
	res := f.command("", c, id, cmd, args...)
	defer res.Release()
	return errorTexts(res)
}

func errorTexts(res *kind.CommandResult) []string {
	var names []string
	for _, be := range res.Errors {
		names = append(names, be.Name+res.Arena.Format(be.Args.Value()))
	}
	return names
}

// load returns a record's state (in the arena f.a), version and existence.
func (f *fixture) load(id reasonr.Value, t string) (reasonr.Value, uint64, bool) {
	f.t.Helper()
	st, v, ok, err := f.st.Unit.Load(context.Background(), f.tenant, f.a, f.typ(t), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return st.Value(), v, ok
}

func (f *fixture) field(rec reasonr.Value, t, name string) reasonr.Value {
	f.t.Helper()
	return f.a.Elems(rec)[f.fieldRef(t, name)]
}

func (f *fixture) kind(name string) reasonr.Value {
	f.t.Helper()
	ti := f.s.Type(f.typ("Kind"))
	for i, n := range ti.Enum {
		if n == name {
			return reasonr.Enum(ti.Ref, i)
		}
	}
	f.t.Fatalf("no value %s", name)
	return reasonr.None
}

// units reads the outbox after the position from.
func (f *fixture) units(from port.Position) []kind.CommittedUnit {
	f.t.Helper()
	us, err := f.st.Effects.Read(context.Background(), from, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	return us
}
