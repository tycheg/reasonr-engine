package server

import (
	"context"
	"io"
	"sort"

	"github.com/tycheg/reasonr-engine/internal/config"
	"github.com/tycheg/reasonr-engine/internal/host"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/internal/registry"
	"github.com/tycheg/reasonr-engine/kind"
)

func commandNames() []string {
	commands := registry.Commands()
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// tool is the engine a tool command opened from engine.yaml and a model.
// An engine kind that runs elsewhere (another process) is reached as a tool
// process: its port.KindTool turns the configuration into a client.
type tool struct {
	Config   *config.Config
	Engine   *host.Engine
	Programs *kernel.Programs
}

// openTool reads the configuration and the model, builds the engine as a
// tool process of the command name and brings the store schema to the
// models; test: the test program with fakes. A non-zero code means the
// message is already printed to stderr. The caller closes tool.Engine.
func openTool(ctx context.Context, name, configPath, model string, test bool, stderr io.Writer) (*tool, int) {
	c := newCommon(name, stderr)
	c.adjust = func(cfg *config.Config) error {
		return host.Tool(cfg)
	}
	if code := c.prepare(ctx, []string{"--config", configPath, "--model", model}, stderr, test); code != 0 {
		return nil, code
	}
	return &tool{Config: c.cfg, Engine: c.engine, Programs: c.programs}, 0
}

func init() {
	registry.SetToolOpener(func(ctx context.Context, name, configPath, model string, test bool, stderr io.Writer) (*kind.Tool, int) {
		t, code := openTool(ctx, name, configPath, model, test, stderr)
		if t == nil {
			return nil, code
		}
		return &kind.Tool{Config: host.PortConfig(t.Config), Programs: t.Programs, Host: t.Engine.Host(), Engine: t.Engine.Kind(), Executor: t.Engine.Executor, Store: t.Engine.Store,
			Close: t.Engine.Close}, code
	})
}
