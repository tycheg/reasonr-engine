package kind

import (
	"context"
	"fmt"
	"io"

	"github.com/tycheg/reasonr-engine/internal/kindreg"
)

// RegisterKind registers an engine kind (engine: in engine.yaml); called
// from init of its module (reasonr-stateless).
func RegisterKind(name string, k Kind) { kindreg.RegisterKind(name, k) }

// RegisterDriver adds a test run driver to reasonrd test (--driver name);
// called from init of its module (the web driver).
func RegisterDriver(name string, d TestDriver) { kindreg.RegisterDriver(name, d) }

// Tool is an engine a command opened from engine.yaml and a model: an
// engine kind that runs elsewhere (another process) is reached as a tool
// process (KindTool turns its configuration into a client).
type Tool struct {
	Config   *Config
	Programs Programs
	Host     Host
	// Engine is the engine the kind opened; Executor is its executor, Store
	// the main store.
	Engine   Engine
	Executor Executor
	Store    UnitStore
	Close    func() error
}

// OpenTool reads the configuration and the model and opens the engine as a
// tool process of the command name (a non-zero code: the message is
// already printed to stderr); test — the test program with fakes.
func OpenTool(ctx context.Context, name, configPath, model string, test bool, stderr io.Writer) (*Tool, int) {
	open, _ := kindreg.ToolOpener().(func(ctx context.Context, name, configPath, model string, test bool, stderr io.Writer) (*Tool, int))
	if open == nil {
		fmt.Fprintln(stderr, "reasonrd: tools need the server (server.Main)")
		return nil, 2
	}
	return open(ctx, name, configPath, model, test, stderr)
}
