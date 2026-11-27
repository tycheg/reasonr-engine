// Package check checks the engine configuration against the tenant models
// (RFC §30–31, reasonrd check): what would otherwise show up only when the
// server starts or in operation.
package check

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/config"
	"github.com/tycheg/reasonr-engine/internal/registry"
)

// Problem is an error or a warning found; Line is the engine.yaml line (0
// — the place is not known).
type Problem struct {
	Code     string
	Severity string // error, warning
	Line     int
	Message  string
}

// Check codes.
const (
	AsyncUnique  = "C0001" // an asynchronous projection with a unique index
	AsyncChecked = "C0002" // an asynchronous projection whose rows commands check
	NoProjection = "C0003" // a projection in projections that the models do not have
	NoTransport  = "C0004" // an external system has no transport
	NoSystem     = "C0005" // a system in integrations that the models do not have
	// C0006 (a bus.publish event the models do not have) is retired with
	// bus.publish: every event is published (RFC §31).
	NoKMS        = "C0007" // personal data without crypto.kms
	NoPersonal   = "C0008" // personal data in a store that does not encrypt it
	NoForgetLog  = "C0009" // the forget journal is not kept or is in the main store
	FakeNoTest   = "C0010" // the fake integration transport outside test mode
	AnyNoSystems = "C0011" // JWT with tenant.any, but the models' external systems have no roles
	NoSigner     = "C0012" // the http integration transport without integrations.auth
	BusNoEvents  = "C0013" // a bus without an event store
	NoUIDir      = "C0014" // ingress.ui.dir does not exist or has no index.html
	BadOIDC      = "C0015" // ingress.ui.auth.oidc without issuer or client_id; OIDC without auth: jwt
	NoPrimitive  = "C0016" // the web runtime build has no implementation of an app primitive
)

// Options are the check conditions.
type Options struct {
	// Test is the server's test mode (reasonrd --test): KMS memory without
	// crypto, no forget journal needed.
	Test bool
}

// Check checks the configuration cfg against the model programs by model
// name (base or a tenant's extension).
func Check(cfg *config.Config, models map[string]*reasonr.Model, opts Options) []Problem {
	c := &checker{cfg: cfg}
	names := make([]string, 0, len(models))
	for n := range models {
		names = append(names, n)
	}
	sort.Strings(names)
	type projection struct {
		ti      *reasonr.TypeInfo
		checked string // the function that reads the projection in commands
	}
	projections := map[string]*projection{}
	externals := map[string]bool{} // systems that have requests
	systemRoles := false           // some external system has a role
	for _, name := range names {
		p := models[name]
		s := p.Schema()
		for i := range s.Types {
			ti := &s.Types[i]
			switch ti.Kind {
			case reasonr.TypeProjection:
				if projections[ti.Name] == nil {
					projections[ti.Name] = &projection{ti: ti}
				}
			case reasonr.TypeExternal:
				systemRoles = systemRoles || ti.Role != ""
				for _, mi := range ti.Methods {
					if mi.Kind == reasonr.MethodRequest {
						externals[ti.Short()] = true
					}
				}
			}
		}
		for proj, fn := range p.CommandReads() {
			if pr := projections[proj]; pr != nil && pr.checked == "" {
				pr.checked = fn
			}
		}
	}
	known := map[string]bool{}
	for name := range projections {
		known[name] = true
	}
	for name := range cfg.Projections {
		if name != "default" && !config.KnownProjection(name, known) {
			c.add(NoProjection, "error", cfg.Line("projections", name), "the tenant models have no projection %s", name)
		}
	}
	pnames := make([]string, 0, len(projections))
	for n := range projections {
		pnames = append(pnames, n)
	}
	sort.Strings(pnames)
	for _, name := range pnames {
		store, key, _ := cfg.Projection(name)
		if store == cfg.State {
			continue
		}
		line := cfg.Line("projections", key)
		pr := projections[name]
		for _, in := range pr.ti.Indexes {
			if in.Unique {
				c.add(AsyncUnique, "error", line, "projection %s with unique index %s can only be synchronous: keep it in %q",
					name, in.Name, cfg.State)
			}
		}
		if pr.checked != "" {
			c.add(AsyncChecked, "error", line, "rows of projection %s are checked by %s: it can only be synchronous (in %q)",
				name, pr.checked, cfg.State)
		}
	}
	_, hasDefault := cfg.Integrations["default"]
	for name := range cfg.Integrations {
		if name != "default" && !externals[name] {
			c.add(NoSystem, "error", cfg.Line("integrations", name), "the tenant models have no external system %s with requests", name)
		}
	}
	sys := make([]string, 0, len(externals))
	for n := range externals {
		sys = append(sys, n)
	}
	sort.Strings(sys)
	for _, name := range sys {
		if _, ok := cfg.Integrations[name]; !ok && !hasDefault {
			c.add(NoTransport, "warning", cfg.Line("integrations"), "external system %s has no transport (integrations.default or integrations.%s): its requests will be dropped",
				name, name)
		}
	}
	c.integrations(systemRoles, opts)
	c.personal(models, opts)
	c.ui()
	uses := map[string][]reasonr.PrimitiveUse{}
	for name, m := range models {
		if u := m.Primitives("web"); len(u) > 0 {
			uses[name] = u
		}
	}
	c.primitives(uses)
	if cfg.Bus != nil && cfg.Events == "" {
		c.add(BusNoEvents, "error", cfg.Line("bus"), "the bus reads events from the event store: set events — the event store")
	}
	sort.SliceStable(c.out, func(i, j int) bool { return c.out[i].Line < c.out[j].Line })
	return c.out
}

type checker struct {
	cfg *config.Config
	out []Problem
}

// integrations checks the transports and JWT of the integration service
// (RFC §18): a fake only in test mode, HTTP with the engine's signature, a
// token for every tenant when there are systems with roles.
func (c *checker) integrations(systemRoles bool, opts Options) {
	cfg := c.cfg
	names := make([]string, 0, len(cfg.Integrations))
	for n := range cfg.Integrations {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		switch config.Type(cfg.Integrations[name]) {
		case "fake":
			if !opts.Test {
				c.add(FakeNoTest, "error", cfg.Line("integrations", name), "integrations.%s: the fake transport is only for reasonrd --test", name)
			}
		case "http":
			if cfg.IntegrationsAuth == nil && !opts.Test {
				c.add(NoSigner, "error", cfg.Line("integrations", name),
					"integrations.%s: HTTP without integrations.auth — the integration service cannot check that a request comes from the engine: set a JWT signing key (ES256)", name)
			}
		}
	}
	for i, n := range cfg.Auth {
		if config.Type(n) != "jwt" || systemRoles {
			continue
		}
		if t := n.Get("tenant"); t != nil && t.Get("any") != nil {
			c.add(AnyNoSystems, "warning", t.Get("any").Line,
				"auth[%d]: tenant.any is an external system token for every tenant, but the models' external systems have no roles (external(role)): such a token allows nothing", i)
		}
	}
}

// ui checks the interface of HTTP ingresses (RFC §22): the runtime build
// directory and OIDC sign-in.
func (c *checker) ui() {
	jwt := false
	for _, n := range c.cfg.Auth {
		jwt = jwt || config.Type(n) == "jwt"
	}
	for i, in := range c.cfg.Ingress {
		ui := in.Get("ui")
		if ui == nil {
			continue
		}
		if d := ui.Get("dir"); d != nil {
			dir, _ := ui.String("dir", "")
			if st, err := os.Stat(filepath.Join(dir, "index.html")); err != nil || st.IsDir() {
				c.add(NoUIDir, "error", d.Line, "ingress[%d].ui.dir: %q has no index.html — build the runtime (npm run build) or remove dir to serve the built-in build", i, dir)
			}
		}
		o := ui.Get("auth")
		if o != nil {
			o = o.Get("oidc")
		}
		if o == nil {
			continue
		}
		issuer, _ := o.String("issuer", "")
		client, _ := o.String("client_id", "")
		switch {
		case issuer == "" || client == "":
			c.add(BadOIDC, "error", o.Line, "ingress[%d].ui.auth.oidc: issuer and client_id are needed", i)
		case !jwt:
			c.add(BadOIDC, "error", o.Line, "ingress[%d].ui.auth.oidc: auth: jwt checks the IdP token — add it to auth", i)
		}
	}
}

// primitives warns about the primitives of web apps (uses by model name) whose implementation is
// not in the ingress runtime build (C0016): the ui.dir directory with the
// reasonr-ui.json manifest, or the built-in build — the standard catalog.
// The automatic UI draws such a primitive.
func (c *checker) primitives(uses map[string][]reasonr.PrimitiveUse) {
	if len(uses) == 0 {
		return
	}
	names := make([]string, 0, len(uses))
	for n := range uses {
		names = append(names, n)
	}
	sort.Strings(names)
	for i, in := range c.cfg.Ingress {
		if config.Type(in) != "http" {
			continue
		}
		std, own, line, ok := c.bundle(i, in)
		if !ok {
			continue
		}
		var missing []string
		seen := map[string]bool{}
		for _, name := range names {
			for _, p := range uses[name] {
				if !own[p.Name] && !(std && p.Std) && !seen[p.Name] {
					seen[p.Name] = true
					missing = append(missing, p.Name)
				}
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			c.add(NoPrimitive, "warning", line, "ingress[%d]: the web runtime build has no implementations of %s — the automatic UI draws them",
				i, strings.Join(missing, ", "))
		}
	}
}

// bundle reads the manifest of the ingress runtime build: whether it has
// the standard catalog and own primitives. Without ui.dir — the built-in
// build (the catalog).
func (c *checker) bundle(i int, in *config.Node) (std bool, own map[string]bool, line int, ok bool) {
	own = map[string]bool{}
	ui := in.Get("ui")
	if ui != nil && ui.Get("proxy") != nil {
		return false, nil, 0, false // a dev server: it checks the implementations
	}
	if ui == nil || ui.Get("dir") == nil {
		return true, own, in.Line, true
	}
	dir, _ := ui.String("dir", "")
	line = ui.Get("dir").Line
	data, err := os.ReadFile(filepath.Join(dir, "reasonr-ui.json"))
	if err != nil {
		if _, statErr := os.Stat(filepath.Join(dir, "index.html")); statErr == nil {
			c.add(NoPrimitive, "warning", line, "ingress[%d].ui.dir: no reasonr-ui.json — build the runtime with the @tycheg/reasonr-vite plugin to check the primitive implementations", i)
		}
		return false, nil, 0, false
	}
	var m struct {
		Std        bool     `json:"std"`
		Primitives []string `json:"primitives"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		c.add(NoPrimitive, "warning", line, "ingress[%d].ui.dir: reasonr-ui.json: %v", i, err)
		return false, nil, 0, false
	}
	for _, p := range m.Primitives {
		own[p] = true
	}
	return m.Std, own, line, true
}

// personal checks the personal data configuration (RFC §20): the KMS, the
// stores that encrypt it, and the forget journal outside the main store.
func (c *checker) personal(models map[string]*reasonr.Model, opts Options) {
	found, events := false, false
	projections := map[string]bool{} // projections with personal fields
	for _, p := range models {
		for i := range p.Schema().Types {
			ti := &p.Schema().Types[i]
			for _, fi := range ti.Fields {
				if fi.Personal != nil {
					found = true
					if ti.Kind == reasonr.TypeProjection {
						projections[ti.Name] = true
					}
				}
			}
			for _, mi := range ti.Methods {
				for _, pi := range mi.Params {
					if pi.Personal != nil {
						found = true
						events = events || mi.Kind == reasonr.MethodEvent
					}
				}
			}
		}
	}
	if !found {
		return
	}
	cfg := c.cfg
	if cfg.Crypto.KMS == nil && !opts.Test {
		c.add(NoKMS, "error", cfg.Line("crypto"), "the models have personal data, but crypto.kms is not set: the store cannot write it")
	}
	store := func(name, what string, line int) {
		n := cfg.Stores[name]
		if n == nil {
			return
		}
		typ := config.Type(n)
		if has, known := registry.StoreHas(typ, registry.PersonalData); known && !has {
			c.add(NoPersonal, "error", line, "%s: store %q (type: %s) does not encrypt personal data", what, name, typ)
		}
	}
	store(cfg.State, "state", cfg.Line("state"))
	names := make([]string, 0, len(projections))
	for n := range projections {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		target, key, _ := cfg.Projection(name)
		line := cfg.Line("projections", key)
		if target != cfg.State {
			store(target, "projection "+name, line)
		}
	}
	if events && cfg.Events != "" && cfg.Events != cfg.State {
		store(cfg.Events, "event store", cfg.Line("events"))
	}
	if opts.Test {
		return
	}
	switch cfg.Crypto.ForgetLog {
	case "":
		c.add(NoForgetLog, "warning", cfg.Line("crypto"), "crypto.forget_log is not set: restoring the main database from a backup brings forgotten subjects back (reasonrd pii replay)")
	case cfg.State:
		c.add(NoForgetLog, "warning", cfg.Line("crypto", "forget_log"), "the forget journal is in the main store %q: a backup of the main database brings back both the forgotten subjects and a journal without them", cfg.State)
	}
}

func (c *checker) add(code, severity string, line int, format string, args ...any) {
	c.out = append(c.out, Problem{Code: code, Severity: severity, Line: line, Message: fmt.Sprintf(format, args...)})
}

// HasErrors reports whether the problems have errors.
func HasErrors(ps []Problem) bool {
	for _, p := range ps {
		if p.Severity == "error" {
			return true
		}
	}
	return false
}

// Text is a problem for a person: "file:line: error C0001: message".
func (p Problem) Text(file string) string {
	sev := "error"
	if p.Severity == "warning" {
		sev = "warning"
	}
	where := file
	if p.Line > 0 {
		where = fmt.Sprintf("%s:%d", file, p.Line)
	}
	return strings.Join([]string{where, ": ", sev, " ", p.Code, ": ", p.Message}, "")
}
