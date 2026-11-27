// Package server is the ReasonR engine server (the reasonrd command) (RFC
// §30–31): it builds the kernel and adapters from engine.yaml, loads the
// models (the base one and with extensions), brings the store schema up to
// the models and runs the process roles.
//
//	reasonrd [serve] --model <module> [--config engine.yaml] [--roles api,timers,dispatch] [--test] [--listen <address>]
//	reasonrd dev [--config engine.yaml] [--integrations <System>=<url>,…] [--ui <dir>|<url>] [--listen <address>] [<module>]
//	reasonrd test [--driver memory,api,web] [--url <address> | --config engine.yaml] [--extension <extension>|base] [<module>|<file>[:<line>] ...]
//	reasonrd check [--config engine.yaml] [--test] [--format text|json] [<module>]
//	reasonrd openapi --integrations [--extension <extension>] [<module>]
//	reasonrd migrate --model <module> [--config engine.yaml] [--run <migration>] [--tenant <tenant>]
//	reasonrd topics --model <module> [--config engine.yaml] [--test] [--format text|json]
//	reasonrd timers retry --model <module> [--config engine.yaml] [--tenant <tenant>]
//	reasonrd dispatch retry --model <module> [--config engine.yaml] [--consumer <consumer>]
//	reasonrd pii rotate --model <module> [--config engine.yaml] [--from <engine.yaml with the previous crypto.kms>]
//	reasonrd pii replay --model <module> [--config engine.yaml] --since <backup time>
//	reasonrd feature list|on|off|log [<flag>] --model <module> [--config engine.yaml] [level]
//
// The package imports no adapters with external drivers: the binary
// chooses their set — the standard reasonrd of the distribution or one
// built from a manifest with reasonr-build.
package server

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/telemetry"
	_ "github.com/tycheg/reasonr-engine/internal/builtin"
	"github.com/tycheg/reasonr-engine/internal/check"
	"github.com/tycheg/reasonr-engine/internal/config"
	"github.com/tycheg/reasonr-engine/internal/host"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/internal/registry"
	"github.com/tycheg/reasonr-engine/internal/servertest"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Main is the binary's entry point: the subcommand from args, stopping on
// SIGINT and SIGTERM; it returns the exit code.
func Main(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return Run(ctx, args, os.Stdout, os.Stderr)
}

// Run chooses the subcommand: its output goes to stdout (reasonrd
// topics), messages and the log to stderr; it returns the exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return runWith(ctx, args, stdout, stderr, nil)
}

// The test servers start through runWith: ready gets the built engine
// before the roles start; tests build engines from loadPrograms.
func init() { servertest.Serve, servertest.LoadPrograms = runWith, loadPrograms }

func runWith(ctx context.Context, args []string, stdout, stderr io.Writer, ready func(*host.Engine)) int {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "serve":
		return serve(ctx, args, stderr, ready)
	case "dev":
		return dev(ctx, args, stderr, ready)
	case "migrate":
		return migrate(ctx, args, stderr)
	case "topics":
		return topics(args, stdout, stderr)
	case "pii":
		return piiCommand(ctx, args, stderr)
	case "feature":
		return featureCommand(ctx, args, stdout, stderr)
	case "test":
		return testCommand(args, stdout, stderr)
	case "check":
		return checkCommand(args, stdout, stderr)
	case "openapi":
		return openapiCommand(args, stdout, stderr)
	case "api":
		return apiCommand(ctx, args, stdout, stderr)
	case "mcp":
		return mcpCommand(ctx, args, os.Stdin, stdout, stderr)
	case "demo":
		return demoCommand(ctx, args, stdout, stderr)
	case "perf":
		return perfCommand(ctx, args, stdout, stderr)
	case "test-worker":
		return workerCommand(args, stdout, stderr)
	}
	if run := registry.Commands()[cmd]; run != nil {
		return run(ctx, args, stdout, stderr)
	}
	names := append([]string{"serve", "dev", "migrate", "test", "check", "openapi", "topics",
		"pii rotate", "pii replay", "feature", "api usage", "mcp", "demo", "perf", "test-worker"}, commandNames()...)
	fmt.Fprintf(stderr, "reasonrd: unknown command %q (%s)\n", cmd, strings.Join(names, ", "))
	return 2
}

// common holds the shared flags and the built engine.
type common struct {
	fs       *flag.FlagSet
	cfgPath  *string
	modelDir *string
	listen   *string // the http ingress address instead of the configuration's; nil — no flag
	// adjust changes the configuration after reading it (reasonrd dev); nil — none.
	adjust   func(*config.Config) error
	cfg      *config.Config
	log      *slog.Logger
	models   []*modelInfo
	module   *reasonr.Module
	programs *kernel.Programs
	engine   *host.Engine
	// snap is the snapshot of the model files the engine was built from
	// (reasonrd dev).
	snap string
}

func newCommon(name string, stderr io.Writer) *common {
	fs := flag.NewFlagSet("reasonrd "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return &common{fs: fs,
		cfgPath:  fs.String("config", "", "the engine configuration engine.yaml (by default everything is in memory, HTTP on :8080)"),
		modelDir: fs.String("model", "", "the model module directory"),
	}
}

// prepare reads the configuration and models, builds the engine and brings
// the store schema up to the models. An exit code != 0 — the message is
// already printed.
func (c *common) prepare(ctx context.Context, args []string, stderr io.Writer, test bool) int {
	if code := c.load(args, stderr, test); code != 0 {
		return code
	}
	return c.build(ctx, stderr, test)
}

// load reads the configuration and models and checks them (reasonrd check).
func (c *common) load(args []string, stderr io.Writer, test bool) int {
	if err := c.fs.Parse(args); err != nil {
		return 2
	}
	if *c.modelDir == "" {
		fmt.Fprintln(stderr, "reasonrd: set --model <module directory>")
		return 2
	}
	c.cfg = config.Default()
	if *c.cfgPath != "" {
		var err error
		if c.cfg, err = loadConfig(*c.cfgPath); err != nil {
			fmt.Fprintf(stderr, "reasonrd: %v\n", err)
			return 2
		}
	}
	if c.adjust != nil {
		if err := c.adjust(c.cfg); err != nil {
			fmt.Fprintf(stderr, "reasonrd: %v\n", err)
			return 2
		}
	}
	if c.listen != nil && *c.listen != "" {
		if err := c.cfg.Listen(*c.listen); err != nil {
			fmt.Fprintf(stderr, "reasonrd: --listen: %v\n", err)
			return 2
		}
	}
	var err error
	if c.log, err = telemetry.Logger(stderr, c.cfg.Logs.Format, c.cfg.Logs.Level); err != nil {
		fmt.Fprintf(stderr, "reasonrd: %v\n", err)
		return 2
	}
	if c.module, c.programs, c.models, err = loadModels(*c.modelDir, c.cfg.Tenants, test, stderr); err != nil {
		fmt.Fprintf(stderr, "reasonrd: %v\n", err)
		return 1
	}
	models := map[string]*reasonr.Model{}
	for _, mi := range c.models {
		models[mi.name] = mi.model
	}
	problems := check.Check(c.cfg, models, check.Options{Test: test})
	for _, p := range problems {
		if p.Severity == "error" {
			fmt.Fprintln(stderr, p.Text(c.configName()))
		} else {
			c.log.Warn(p.Text(c.configName()))
		}
	}
	if check.HasErrors(problems) {
		fmt.Fprintln(stderr, "reasonrd: the configuration does not fit the models (reasonrd check)")
		return 2
	}
	return 0
}

// build builds the engine and brings the store schema up to the models.
func (c *common) build(ctx context.Context, stderr io.Writer, test bool) int {
	if test {
		c.log.Warn("TEST MODE: Test authentication accepts any caller")
	}
	models := map[string]reasonr.Interpreter{}
	for _, mi := range c.models {
		models[mi.name] = mi.model
	}
	if test {
		c.addTestModels(models, stderr)
	}
	var err error
	if c.engine, err = host.Build(c.cfg, c.programs, host.Options{Logger: c.log, Test: test, Models: models, UI: c.uiCatalog()}); err != nil {
		fmt.Fprintf(stderr, "reasonrd: %v\n", err)
		return 2
	}
	if code := c.ensureSchemas(ctx, stderr); code != 0 {
		_ = c.engine.Close()
		return code
	}
	return 0
}

// addTestModels adds the models of every extension of the module: in test
// mode an ephemeral tenant may take any of them.
func (c *common) addTestModels(models map[string]reasonr.Interpreter, stderr io.Writer) {
	if c.module == nil {
		return // a model file holds one model
	}
	for _, ext := range c.module.Extensions() {
		if models[ext] != nil {
			continue
		}
		m, err := c.module.Build(reasonr.Options{Extension: ext, Test: true})
		if err != nil {
			fmt.Fprintf(stderr, "reasonrd: model %s is not loaded for test mode: %v\n", ext, err)
			continue
		}
		models[ext] = m
		// The store schema — for models that have no tenants yet too.
		c.models = append(c.models, &modelInfo{name: ext, model: m})
	}
}

func (c *common) configName() string {
	if *c.cfgPath == "" {
		return "engine.yaml"
	}
	return *c.cfgPath
}

// ensureSchemas checks each model's compatibility with its previous
// version in each store that needs a schema (RFC §32), and creates the
// missing tables and indexes.
func (c *common) ensureSchemas(ctx context.Context, stderr io.Writer) int {
	names := make([]string, 0, len(c.engine.Stores))
	for name := range c.engine.Stores {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		ss, ok := c.engine.Stores[name].(ports.SchemaStore)
		if !ok {
			continue
		}
		if code := c.ensureStore(ctx, ss, stderr); code != 0 {
			return code
		}
	}
	return 0
}

func (c *common) ensureStore(ctx context.Context, ss ports.SchemaStore, stderr io.Writer) int {
	for _, mi := range c.models {
		prev, err := ss.LastSchema(ctx, mi.name)
		if err != nil {
			fmt.Fprintf(stderr, "reasonrd: schema %s: %v\n", mi.name, err)
			return 1
		}
		if prev != nil {
			if errs := mi.model.Compatible(prev); errs != nil {
				stderr.Write(errs.Text)
				fmt.Fprintf(stderr, "reasonrd: model %s is incompatible with the stored data: a manual migration is needed (RFC §32)\n", mi.name)
				return 1
			}
		}
		if err := ss.EnsureSchema(ctx, mi.name, mi.model.Schema(), mi.tenants); err != nil {
			fmt.Fprintf(stderr, "reasonrd: schema %s: %v\n", mi.name, err)
			return 1
		}
	}
	return 0
}

func serve(ctx context.Context, args []string, stderr io.Writer, ready func(*host.Engine)) int {
	c := newCommon("serve", stderr)
	roles := c.fs.String("roles", "", "process roles: api, dispatch and the engine kind's (stateless: timers, dispatch); by default — all")
	test := c.fs.Bool("test", false, "test mode: Test authentication (never in production)")
	c.listen = c.fs.String("listen", "", "the http ingress address instead of the configuration's, for example 127.0.0.1:8081")
	if code := c.prepare(ctx, args, stderr, *test || hasFlag(args, "test")); code != 0 {
		return code
	}
	defer c.engine.Close()
	if err := c.engine.EnsureTopics(ctx, false); err != nil {
		fmt.Fprintf(stderr, "reasonrd: %v\n", err)
		return 2
	}
	if ready != nil {
		ready(c.engine)
	}
	run := c.engine.Roles()
	if *roles != "" {
		run = strings.Split(*roles, ",")
	}
	if err := c.engine.Run(ctx, run); err != nil && !errors.Is(err, context.Canceled) {
		c.log.Error("the engine stopped with an error", "error", err)
		return 1
	}
	c.log.Info("engine stopped")
	return 0
}

// hasFlag reports whether a boolean flag is set before parsing (test mode
// is needed already when the engine is built).
func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == "-"+name || a == "--"+name || a == "--"+name+"=true" || a == "-"+name+"=true" {
			return true
		}
	}
	return false
}

// migrate brings the store schema up to the models and, with --run, runs
// the manual migration for the tenants whose model has it.
func migrate(ctx context.Context, args []string, stderr io.Writer) int {
	c := newCommon("migrate", stderr)
	name := c.fs.String("run", "", "a manual migration of the model")
	tenant := c.fs.String("tenant", "", "a data tenant; by default — all with this migration")
	// An engine that runs elsewhere (another process): migrate works through
	// it as a tool process, a manual migration goes through the kind; when the
	// configuration does not say how to reach it, only the store schema.
	c.adjust = func(cfg *config.Config) error {
		if host.HasTool(cfg) && host.Tool(cfg) != nil {
			cfg.Engine = "stateless"
		}
		return nil
	}
	if code := c.prepare(ctx, args, stderr, false); code != 0 {
		return code
	}
	defer c.engine.Close()
	if err := c.engine.EnsureTopics(ctx, true); err != nil {
		fmt.Fprintf(stderr, "reasonrd: %v\n", err)
		return 2
	}
	if *name == "" {
		c.log.Info("the store schema is brought up to the models", "models", len(c.models))
		return 0
	}
	ran := 0
	for _, t := range c.programs.Tenants() {
		if *tenant != "" && string(t) != *tenant {
			continue
		}
		sc, _ := c.programs.Schema(t)
		if !hasMigration(sc.Migrations, *name) {
			continue
		}
		m, ok := c.engine.Executor.(kind.Maintainer)
		if !ok {
			fmt.Fprintf(stderr, "reasonrd: the engine kind runs no data migrations\n")
			return 2
		}
		if err := m.Migrate(ctx, t, *name); err != nil {
			fmt.Fprintf(stderr, "reasonrd: tenant %s: %v\n", t, err)
			return 1
		}
		c.log.Info("migration done", "migration", *name, "tenant", string(t))
		ran++
	}
	if ran == 0 {
		fmt.Fprintf(stderr, "reasonrd: the models of the chosen tenants have no migration %q\n", *name)
		return 1
	}
	return 0
}

// topics prints the broker topics the bus and the request transports need,
// to create them outside the engine (Terraform, GitOps). It does not
// connect to the stores or the broker.
func topics(args []string, stdout, stderr io.Writer) int {
	c := newCommon("topics", stderr)
	test := c.fs.Bool("test", false, "the topics of test mode (models with fakes)")
	format := c.fs.String("format", "text", "format: text — a name per line, json — with the component and the create_topics settings")
	if code := c.load(args, stderr, hasFlag(args, "test")); code != 0 {
		return code
	}
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "reasonrd: unknown format %q (text, json)\n", *format)
		return 2
	}
	ts, err := host.Topics(c.cfg, c.programs, host.Options{Logger: c.log, Test: *test})
	if err != nil {
		fmt.Fprintf(stderr, "reasonrd: %v\n", err)
		return 2
	}
	if *format == "text" {
		for _, t := range ts {
			fmt.Fprintln(stdout, t.Name)
		}
		return 0
	}
	type topic struct {
		Name        string            `json:"name"`
		Component   string            `json:"component"`
		Partitions  int               `json:"partitions,omitempty"`
		Replication int               `json:"replication,omitempty"`
		Config      map[string]string `json:"config,omitempty"`
	}
	out := struct {
		Version int     `json:"version"`
		Topics  []topic `json:"topics"`
	}{Version: 1, Topics: []topic{}}
	for _, t := range ts {
		tp := topic{Name: t.Name, Component: t.Component}
		if s := t.Settings; s != nil {
			tp.Partitions, tp.Replication, tp.Config = s.Partitions, s.Replication, s.Config
		}
		out.Topics = append(out.Topics, tp)
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintf(stderr, "reasonrd: %v\n", err)
		return 2
	}
	return 0
}

func hasMigration(list []string, name string) bool {
	for _, m := range list {
		if m == name {
			return true
		}
	}
	return false
}

// modelInfo is a compiled model and the data tenants that use it.
type modelInfo struct {
	name    string
	model   *reasonr.Model
	tenants []port.Tenant
}

// loadModels compiles the models of data tenants: base is the base model,
// otherwise the extension with that name. A model several data tenants
// need is compiled once. In test mode (test) — the test program with fakes
// of external systems, otherwise the production one.
func loadModels(dir string, tenants map[string]string, test bool, stderr io.Writer) (*reasonr.Module, *kernel.Programs, []*modelInfo, error) {
	if info, err := os.Stat(dir); err == nil && !info.IsDir() {
		programs, models, err := openModel(dir, tenants, test)
		return nil, programs, models, err
	}
	mod, err := reasonr.Load(dir)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("model %s: %w", dir, err)
	}
	byName := map[string]*modelInfo{}
	programs := kernel.NewPrograms()
	names := make([]string, 0, len(tenants))
	for t := range tenants {
		names = append(names, t)
	}
	sort.Strings(names)
	var models []*modelInfo
	for _, tenant := range names {
		model := tenants[tenant]
		mi := byName[model]
		if mi == nil {
			ext := model
			if model == "base" {
				ext = ""
			}
			m, err := mod.Build(reasonr.Options{Extension: ext, Test: test})
			var errs *reasonr.Errors
			if errors.As(err, &errs) {
				stderr.Write(errs.Text)
				return nil, nil, nil, fmt.Errorf("model %s has errors", model)
			}
			if err != nil {
				return nil, nil, nil, fmt.Errorf("model %s: %w", model, err)
			}
			mi = &modelInfo{name: model, model: m}
			byName[model] = mi
			models = append(models, mi)
		}
		mi.tenants = append(mi.tenants, port.Tenant(tenant))
		programs.Set(port.Tenant(tenant), mi.model)
	}
	return mod, programs, models, nil
}

// loadConfig reads engine.yaml and checks that its engine kind is linked
// into this binary.
func loadConfig(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if _, err := registry.Kind(cfg.Engine); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// loadPrograms compiles the models of the data tenants like the server does
// at start (base is the base model, other names are extensions; test: the
// test program with fakes). Tools and tests built on the server use it.
func loadPrograms(dir string, tenants map[string]string, test bool) (*kernel.Programs, error) {
	_, programs, _, err := loadModels(dir, tenants, test, io.Discard)
	return programs, err
}

// openModel opens a model file (reasonr build -o): one model, base or an
// extension, for every data tenant.
func openModel(path string, tenants map[string]string, test bool) (*kernel.Programs, []*modelInfo, error) {
	m, err := reasonr.Open(path)
	if err != nil {
		return nil, nil, err
	}
	if m.Test() != test {
		want := "a production model (reasonr build -o)"
		if test {
			want = "a test model (reasonr build --test -o)"
		}
		return nil, nil, fmt.Errorf("%s: the engine needs %s", path, want)
	}
	name := cmp.Or(m.Extension(), "base")
	mi := &modelInfo{name: name, model: m}
	programs := kernel.NewPrograms()
	for tenant, model := range tenants {
		if model != name {
			return nil, nil, fmt.Errorf("%s holds model %s, tenant %s needs %s", path, name, tenant, model)
		}
		mi.tenants = append(mi.tenants, port.Tenant(tenant))
		programs.Set(port.Tenant(tenant), m)
	}
	sort.Slice(mi.tenants, func(i, j int) bool { return mi.tenants[i] < mi.tenants[j] })
	return programs, []*modelInfo{mi}, nil
}
