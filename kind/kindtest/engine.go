package kindtest

import (
	"context"
	"sync"
	"testing"

	reasonr "github.com/tycheg/reasonr-compiler"
	_ "github.com/tycheg/reasonr-engine/internal/builtin" // the stores and the KMS in memory
	"github.com/tycheg/reasonr-engine/internal/config"
	"github.com/tycheg/reasonr-engine/internal/host"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Engine is an engine a test runs from the text of engine.yaml.
type Engine struct {
	// Host is the shared part of the engine (its stores: Host.Store).
	Host kind.Host
	// Executor runs calls: the kind's executor or the host's kernel.
	Executor kind.Executor
	// Stop stops the engine (a repeated stop does nothing): a test can stop
	// it in the middle.
	Stop func()
}

// Start builds an engine from yaml in test mode with the model m of the
// tenant port.DefaultTenant, creates the store schemas and runs the roles
// the kind's tests need (stateless: timers and dispatch; another kind: its
// own roles). Kinds other than stateless come from the modules the test
// links.
func Start(t testing.TB, yaml string, m reasonr.Interpreter) *Engine {
	t.Helper()
	e, cfg := build(t, yaml, m)
	ctx := context.Background()
	for _, st := range e.Stores {
		if ss, ok := st.(ports.SchemaStore); ok {
			if err := ss.EnsureSchema(ctx, "base", m.Schema(), []port.Tenant{port.DefaultTenant}); err != nil {
				_ = e.Close()
				t.Fatal(err)
			}
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = e.Run(ctx, host.TestRoles(cfg))
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			<-done
			_ = e.Close()
		})
	}
	t.Cleanup(stop)
	return &Engine{Host: e.Host(), Executor: e.Executor, Stop: stop}
}

// NewHost is the host of a stateless engine in memory that does not run:
// what a kind's parts are built with in tests.
func NewHost(t testing.TB) kind.Host {
	t.Helper()
	e, _ := build(t, "engine: stateless\n", nil)
	t.Cleanup(func() { _ = e.Close() })
	return e.Host()
}

func build(t testing.TB, yaml string, m reasonr.Interpreter) (*host.Engine, *config.Config) {
	cfg, err := config.Decode([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	UseKind()
	programs := kernel.NewPrograms()
	models := map[string]reasonr.Interpreter{}
	if m != nil {
		programs.Set(port.DefaultTenant, m)
		models["base"] = m
	}
	e, err := host.Build(cfg, programs, host.Options{Test: true, Models: models})
	if err != nil {
		t.Fatal(err)
	}
	return e, cfg
}
