package server

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/config"
	"github.com/tycheg/reasonr-engine/internal/host"
)

// dev is reasonrd dev [--config engine.yaml] [--integrations System=url,…]
// [--ui dir|url] [--listen address] [module]: the engine for local debugging
// (RFC §33). Always test mode: fakes, Test sign-in, choosing the caller in
// the UI.
//
//   - Without --config everything is in memory; the data tenants are
//     default with the base model and a tenant per extension (acme — model acme).
//   - External systems without a transport in the configuration run on
//     fakes; --integrations is the http transport to the integration
//     service for the named systems (a JWT signing key of its own per run,
//     JWKS on the http ingress).
//   - --ui is an own web runtime build (ui.dir) or a dev server (ui.proxy).
//   - A change of *.rr rebuilds the engine without restarting the process:
//     the in-memory data is reset, real databases keep theirs. Model and
//     configuration errors go to the log, and the previous program keeps running.
func dev(ctx context.Context, args []string, stderr io.Writer, ready func(*host.Engine)) int {
	fl := flag.NewFlagSet("reasonrd dev", flag.ContinueOnError)
	fl.SetOutput(stderr)
	cfgPath := fl.String("config", "", "the engine configuration (the platform's real databases); by default everything is in memory")
	integrations := fl.String("integrations", "", "external systems on the real integration service: System=url,…; the rest run on fakes")
	ui := fl.String("ui", "", "an own web runtime build: a directory (ui.dir) or a dev server address (ui.proxy)")
	listen := fl.String("listen", "", "the http ingress address (by default from the configuration, without one :8080)")
	interval := fl.Duration("watch", 500*time.Millisecond, "how often to check *.rr for changes; 0 — do not watch")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if fl.NArg() > 1 {
		fmt.Fprintln(stderr, "reasonrd dev: set one module")
		return 2
	}
	dir := "."
	if fl.NArg() == 1 {
		dir = fl.Arg(0)
	}
	systems, err := parseSystems(*integrations)
	if err != nil {
		fmt.Fprintf(stderr, "reasonrd dev: --integrations: %v\n", err)
		return 2
	}
	mod, err := reasonr.Load(dir)
	if err != nil {
		fmt.Fprintf(stderr, "reasonrd: %v\n", err)
		return 2
	}
	tenants := mod.Extensions()
	start := func() (*common, int) {
		c := newCommon("dev", stderr)
		// The snapshot comes before the model is compiled: a change made
		// while the engine builds or starts is seen by the watch loop.
		c.snap = snapshot(dir)
		c.listen = listen
		c.adjust = func(cfg *config.Config) error { return devConfig(cfg, *cfgPath != "", tenants, systems, *ui) }
		a := []string{"--model", dir}
		if *cfgPath != "" {
			a = append(a, "--config", *cfgPath)
		}
		return c, c.prepare(ctx, a, stderr, true)
	}
	c, code := start()
	if code != 0 {
		return code
	}
	for c != nil {
		if c, code = c.devRun(ctx, *interval, start, ready); code != 0 {
			return code
		}
	}
	return 0
}

// devRun runs the engine c and watches the module. It returns the engine
// for the next run (a rebuild) or nil — stop.
func (c *common) devRun(ctx context.Context, interval time.Duration, start func() (*common, int), ready func(*host.Engine)) (*common, int) {
	if err := c.engine.EnsureTopics(ctx, false); err != nil {
		c.log.Error("topics not created", "error", err)
		_ = c.engine.Close()
		return nil, 2
	}
	if ready != nil {
		ready(c.engine)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- c.engine.Run(runCtx, c.engine.Roles()) }()
	if in := c.cfg.HTTP(); in != nil {
		addr, _ := in.String("listen", ":8080")
		c.log.Info("reasonrd dev: test mode, UI at http://" + localhost(addr) + "/ui/")
	}
	var tick <-chan time.Time
	if interval > 0 {
		t := time.NewTicker(interval)
		defer t.Stop()
		tick = t.C
	}
	snap := c.snap
	for {
		select {
		case err := <-done:
			_ = c.engine.Close()
			if err != nil && !errors.Is(err, context.Canceled) {
				c.log.Error("the engine stopped with an error", "error", err)
				return nil, 1
			}
			c.log.Info("engine stopped")
			return nil, 0
		case <-tick:
			s := snapshot(*c.modelDir)
			if s == snap {
				continue
			}
			snap = s
			next := c.reload(start)
			if next == nil {
				continue
			}
			stop()
			<-done
			_ = c.engine.Close()
			return next, 0
		}
	}
}

// reload rebuilds the engine on the changed model while the previous one
// runs: in-memory stores hold the model schema, so the program is not
// swapped in a running engine. Model and configuration errors, and with
// real databases an incompatibility with the data, keep the previous
// engine (nil).
func (c *common) reload(start func() (*common, int)) *common {
	c.log.Info("the model changed: rebuilding the engine, the in-memory data is reset")
	next, code := start()
	if code != 0 {
		c.log.Error("the engine is not rebuilt (errors above), the previous program keeps running")
		return nil
	}
	return next
}

// devConfig completes the configuration for reasonrd dev: the data tenants
// of extensions (without a configuration file), default fakes, the http
// transport of the --integrations systems, the --ui build.
func devConfig(cfg *config.Config, fromFile bool, tenants []string, systems map[string]string, ui string) error {
	if !fromFile {
		for _, t := range tenants {
			if _, ok := cfg.Tenants[t]; !ok {
				cfg.Tenants[t] = t
			}
		}
	}
	if cfg.Integrations == nil {
		cfg.Integrations = map[string]*config.Node{}
	}
	if cfg.Integrations["default"] == nil {
		cfg.Integrations["default"] = config.NewMap("type", "fake")
	}
	for s, url := range systems {
		cfg.Integrations[s] = config.NewMap("type", "http", "url", url)
	}
	if ui == "" {
		return nil
	}
	in := cfg.HTTP()
	if in == nil {
		return errors.New("--ui: the ingress has no type: http entry")
	}
	n := in.Get("ui")
	if n == nil {
		n = config.NewMap()
		in.Set("ui", n)
	}
	n.Delete("dir")
	n.Delete("proxy")
	if strings.HasPrefix(ui, "http://") || strings.HasPrefix(ui, "https://") {
		n.Set("proxy", &config.Node{Kind: config.Scalar, Value: ui})
	} else {
		n.Set("dir", &config.Node{Kind: config.Scalar, Value: ui})
	}
	return nil
}

// parseSystems parses System=url,….
func parseSystems(s string) (map[string]string, error) {
	out := map[string]string{}
	if s == "" {
		return out, nil
	}
	for _, kv := range strings.Split(s, ",") {
		name, url, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok || name == "" || url == "" {
			return nil, fmt.Errorf("%q: expected System=url", kv)
		}
		out[name] = url
	}
	return out, nil
}

// snapshot holds the names, sizes and modification times of the module's *.rr files.
func snapshot(dir string) string {
	var b strings.Builder
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && path != dir && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(path, ".rr") {
			if info, err := d.Info(); err == nil {
				fmt.Fprintf(&b, "%s %d %d\n", path, info.Size(), info.ModTime().UnixNano())
			}
		}
		return nil
	})
	return b.String()
}

// localhost is the address for a link: ":8080" — localhost:8080.
func localhost(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "localhost" + addr
	}
	return addr
}
