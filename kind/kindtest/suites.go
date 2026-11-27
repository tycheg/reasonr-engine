package kindtest

import (
	"context"
	"strings"
	"testing"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/memory"
	"github.com/tycheg/reasonr-engine/internal/bus"
	"github.com/tycheg/reasonr-engine/internal/conformance"
	"github.com/tycheg/reasonr-engine/internal/dispatch"
	"github.com/tycheg/reasonr-engine/internal/host"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/internal/pii"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/internal/rows"
	"github.com/tycheg/reasonr-engine/internal/testmodels"
	"github.com/tycheg/reasonr-engine/internal/testrun"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// The conformance suites every adapter of a port runs, and their models.
type (
	// Stores are the ports of one store under test; Factory makes them.
	Stores            = conformance.Stores
	Factory           = conformance.Factory
	ProjectionFactory = conformance.ProjectionFactory
	HubFactory        = conformance.HubFactory
	PublisherFactory  = conformance.PublisherFactory
	// Received is a bus message as the test read it from the broker.
	Received = conformance.Received
)

// The tenants and the start time of the conformance suites.
const (
	T1 = conformance.T1
	T2 = conformance.T2
)

// StartTime is the start time of the suites' clock.
var StartTime = conformance.Start

var (
	RunUnitStore       = conformance.RunUnitStore
	RunRequests        = conformance.RunRequests
	RunPersonal        = conformance.RunPersonal
	RunProjectionStore = conformance.RunProjectionStore
	RunCoordinator     = conformance.RunCoordinator
	RunEntitySink      = conformance.RunEntitySink
	RunHub             = conformance.RunHub
	RunPublisher       = conformance.RunPublisher
	RunStateTransfer   = conformance.RunStateTransfer
	// Machine and PersonalMachine are the models of the suites.
	Machine         = conformance.Machine
	PersonalMachine = conformance.PersonalMachine
	// ExecutorHistory runs one history of calls on executors and gives the
	// responses; CompareHistories compares two (executor conformance).
	ExecutorHistory  = conformance.ExecutorHistory
	CompareHistories = conformance.CompareHistories
)

// ModelDir is the sources of an example model: taxi, bank (the compiler's
// examples) or a model of the engine's own tests. Modules is the
// REASONR_MODULES value for copies of the taxi outside its directory.
var (
	ModelDir = testmodels.Dir
	Modules  = testmodels.Modules
)

// RowStore is a row store (port.RowStore) seen as model values, as the
// engine sees it: the store tests of an adapter run on it.
type RowStore interface {
	kind.UnitStore
	kind.TimerSource
	kind.EffectSource
	kind.ProjectionStore
	kind.EntitySink
	// EnsureSchema and LastSchema bring the store to a model schema.
	EnsureSchema(ctx context.Context, model string, s *reasonr.Schema, tenants []port.Tenant) error
	LastSchema(ctx context.Context, model string) (*reasonr.Schema, error)
	// Append writes the events of units to the event store.
	Append(ctx context.Context, units []kind.CommittedUnit) error
	// Unwrap gives the adapter's store; Close closes it.
	Unwrap() any
	Close() error
}

// Rows wraps the row store s by the catalog c; kms wraps the tenant keys
// of personal data (MemoryKMS in tests).
func Rows(s port.RowStore, c kind.Catalog, kms port.KMS) RowStore {
	return rows.New(s, c, rows.Options{KMS: kms})
}

// MemoryKMS is a KMS with a master key in memory; RandomKey is a random
// data key.
func MemoryKMS() port.KMS { return pii.NewMemoryKMS() }
func RandomKey() []byte   { return pii.RandomKey() }

// RunKMS checks a KMS adapter: a tenant key wrapped with it encrypts
// personal data, and a new keyring (another process) unwraps it again.
func RunKMS(t *testing.T, k port.KMS) {
	ctx := context.Background()
	keys := pii.NewMemoryKeys()
	subj := port.SubjectRef{Type: "Person", ID: `"p1"`}
	sealed, err := pii.NewKeyring(k, keys, pii.Options{}).Sealer("t1").Seal(ctx, nil, subj, "Person", "phone", []byte(`"+79990000001"`))
	if err != nil {
		t.Fatal(err)
	}
	plain, ok, err := pii.NewKeyring(k, keys, pii.Options{}).Sealer("t1").Open(ctx, nil, subj, "Person", "phone", sealed)
	if err != nil || !ok || string(plain) != `"+79990000001"` {
		t.Fatalf("decryption by another process: %q %v %v", plain, ok, err)
	}
}

// NewPrograms is an empty catalog of tenant models; NewKernel a kernel
// over a store; NewClock a clock that starts at start.
func NewPrograms() kind.Programs                         { return kernel.NewPrograms() }
func NewKernel(c kind.KernelConfig) (kind.Kernel, error) { return host.NewKernel(c) }
func NewClock(start time.Time) port.Clock                { return memory.NewClock(start) }

// DefaultNow is the default start time of the in-memory engine's clock.
var DefaultNow = memory.DefaultNow

// MemoryStore is an in-memory store of the catalog c; NewCoordinator an
// in-memory coordinator of the effect stream.
func MemoryStore(c kind.Catalog) kind.MemoryStore { return memory.NewStore(c) }
func NewCoordinator() port.Coordinator            { return memory.NewCoordinator() }

// Backend is a store the model tests run on (RunModelTests).
type Backend = memory.Backend

// RunModelTests runs the tests of the model m on the in-memory engine over
// the stores backend makes (with the determinism check): the number of
// tests, of failed ones and the report text.
func RunModelTests(ctx context.Context, m *reasonr.Model, backend func(kind.Catalog) Backend) (tests, failed int, text string, err error) {
	rep, err := testrun.Run(ctx, m, testrun.Options{CheckDeterminism: true, Backend: backend})
	if err != nil {
		return 0, 0, "", err
	}
	var b strings.Builder
	_ = testrun.WriteText(&b, rep, true)
	return len(rep.Tests), rep.Failed(), b.String(), nil
}

// Topic expands a bus topic template ({tenant}, {kind}, {type}).
var Topic = bus.Topic

// The cursors of the bus: the events consumer the bus waits for, and the bus.
const (
	EventsCursor = dispatch.EventsConsumer
	BusCursor    = dispatch.BusCursor
)

// PublishEvents publishes the due events of feed to pub (made ready, as
// the engine does) with the cursors of coord: the number published.
func PublishEvents(ctx context.Context, feed port.EventFeed, pub port.Publisher, coord port.Coordinator) (int, error) {
	return (&dispatch.Bus{Feed: feed, Publisher: Ready(pub), Coord: coord}).PublishDue(ctx)
}

// ReadyRequest fills what an integration transport gets ready in a request,
// as the engine does before Send.
func ReadyRequest(c kind.Catalog, t port.IntegrationTransport, r *kind.LeasedRequest) error {
	return dispatch.ReadyRequest(c, ports.Plugin{IntegrationTransport: t}, r)
}
