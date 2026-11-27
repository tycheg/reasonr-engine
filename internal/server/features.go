package server

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"os/user"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/tycheg/reasonr-engine/internal/codecs"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/features"
	"github.com/tycheg/reasonr-engine/internal/host"
	"github.com/tycheg/reasonr-engine/internal/testapi"
	"github.com/tycheg/reasonr-engine/port"
)

// Feature flags in production (RFC §23.4): the flags of the models' code
// and their rollout in the features store.
//
//	reasonrd feature list --model <module> [--config engine.yaml] [--format text|json]
//	reasonrd feature on <flag> --model <module> [--config engine.yaml] (--actor Type:id | --role role --tenant t | --tenant t | --all)
//	reasonrd feature off <flag> --model <module> [--config engine.yaml] [--actor Type:id | --role role --tenant t | --tenant t | --all]
//	reasonrd feature log [<flag>] --model <module> [--config engine.yaml]
//
// With --url instead of --model and --config the commands work with the
// rollout of a test mode engine (reasonrd dev): through /_test/features.

const featureUsage = "reasonrd: feature list — the code's flags and their rollout; feature on|off <flag> — switch on or off " +
	"(--actor Type:id, --role role --tenant tenant, --tenant tenant, --all; off without a level — at every level); feature log [<flag>] — the change journal"

// featureCommand — reasonrd feature list|on|off|log.
func featureCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, featureUsage)
		return 2
	}
	switch args[0] {
	case "list":
		return featureList(ctx, args[1:], stdout, stderr)
	case "on", "off":
		return featureSwitch(ctx, args[0] == "on", args[1:], stdout, stderr)
	case "log":
		return featureLog(ctx, args[1:], stdout, stderr)
	}
	fmt.Fprintln(stderr, featureUsage)
	return 2
}

// positional separates a name at the start of the arguments: reasonrd feature on tips --tenant t.
func positional(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

// featureBackend is where the rollout is: the store by engine.yaml or a
// test mode engine by address (reasonrd dev, --url).
type featureBackend interface {
	code() map[string]*codeFeature
	rules(ctx context.Context) ([]port.FeatureRule, error)
	change(ctx context.Context, c port.FeatureChange) error
	log(ctx context.Context, feature string) ([]port.FeatureChange, error)
	close()
}

// openFeatures parses the flags and opens the rollout: with --url — a test
// mode engine, otherwise — the models and the features store by
// engine.yaml (without changing the store schema).
func (c *common) openFeatures(ctx context.Context, args []string, stderr io.Writer) (featureBackend, int) {
	url := c.fs.String("url", "", "the address of a test mode engine (reasonrd dev): the rollout in its store")
	if err := c.fs.Parse(args); err != nil {
		return nil, 2
	}
	if *url != "" {
		b := &remoteFeatures{base: strings.TrimRight(*url, "/"), client: &http.Client{Timeout: 30 * time.Second}}
		if err := b.load(ctx); err != nil {
			fmt.Fprintf(stderr, "reasonrd: %v\n", err)
			return nil, 1
		}
		return b, 0
	}
	if code := c.load(args, stderr, false); code != 0 {
		return nil, code
	}
	models := map[string]reasonr.Interpreter{}
	for _, mi := range c.models {
		models[mi.name] = mi.model
	}
	var err error
	if c.engine, err = host.Build(c.cfg, c.programs, host.Options{Logger: c.log, Models: models}); err != nil {
		fmt.Fprintf(stderr, "reasonrd: %v\n", err)
		return nil, 2
	}
	r := c.engine.Features
	if r.Store == nil {
		_ = c.engine.Close()
		fmt.Fprintf(stderr, "reasonrd: store %q does not keep feature flags (features in engine.yaml)\n", c.cfg.Features)
		return nil, 2
	}
	if err := r.Load(ctx); err != nil {
		_ = c.engine.Close()
		fmt.Fprintf(stderr, "reasonrd: %v\n", err)
		return nil, 1
	}
	return &localFeatures{c: c, r: r}, 0
}

// localFeatures is the rollout in the store by engine.yaml.
type localFeatures struct {
	c *common
	r *features.Resolver
}

func (l *localFeatures) code() map[string]*codeFeature {
	out := map[string]*codeFeature{}
	for _, mi := range l.c.models {
		for _, f := range mi.model.Schema().Features {
			cf := out[f.Name]
			if cf == nil {
				cf = &codeFeature{FeatureInfo: f}
				out[f.Name] = cf
			}
			cf.Models = append(cf.Models, mi.name)
		}
	}
	return out
}

func (l *localFeatures) rules(context.Context) ([]port.FeatureRule, error) { return l.r.Rules(), nil }
func (l *localFeatures) change(ctx context.Context, c port.FeatureChange) error {
	return l.r.Change(ctx, c)
}
func (l *localFeatures) log(ctx context.Context, feature string) ([]port.FeatureChange, error) {
	return l.r.Store.FeatureLog(ctx, feature)
}
func (l *localFeatures) close() { _ = l.c.engine.Close() }

// remoteFeatures is the rollout of a test mode engine through /_test/features.
type remoteFeatures struct {
	base   string
	client *http.Client
	state  testapi.FeatureState
}

func (r *remoteFeatures) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, r.base+path, rd)
	if err != nil {
		return err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d %s (is the engine running in test mode: reasonrd dev?)", method, path, resp.StatusCode, bytes.TrimSpace(data))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (r *remoteFeatures) load(ctx context.Context) error {
	return r.do(ctx, "GET", "/_test/features", nil, &r.state)
}

func (r *remoteFeatures) code() map[string]*codeFeature {
	out := map[string]*codeFeature{}
	for _, f := range r.state.Code {
		out[f.Name] = &codeFeature{FeatureInfo: f.FeatureInfo, Models: f.Models}
	}
	return out
}

func (r *remoteFeatures) rules(context.Context) ([]port.FeatureRule, error) {
	return r.state.Rules, nil
}
func (r *remoteFeatures) change(ctx context.Context, c port.FeatureChange) error {
	return r.do(ctx, "POST", "/_test/features", c, nil)
}
func (r *remoteFeatures) log(ctx context.Context, feature string) ([]port.FeatureChange, error) {
	var out []port.FeatureChange
	err := r.do(ctx, "GET", "/_test/features/log?feature="+neturl.QueryEscape(feature), nil, &out)
	return out, err
}
func (r *remoteFeatures) close() {}

// codeFeature is a code flag: the models that have it.
type codeFeature struct {
	reasonr.FeatureInfo
	Models []string
}

// featureRow is a line of reasonrd feature list.
type featureRow struct {
	Name    string   `json:"name"`
	Context string   `json:"context,omitempty"`
	Date    string   `json:"date,omitempty"`
	AgeDays int      `json:"ageDays"`
	Doc     string   `json:"doc,omitempty"`
	Models  []string `json:"models,omitempty"`
	Rollout []string `json:"rollout"`
	// Removed: the flag is not in the models' code, but its rollout is left in the store.
	Removed bool `json:"removed,omitempty"`
}

func featureList(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	c := newCommon("feature list", stderr)
	format := c.fs.String("format", "text", "format: text or json")
	b, code := c.openFeatures(ctx, args, stderr)
	if code != 0 {
		return code
	}
	defer b.close()
	if *format != "text" && *format != "json" {
		fmt.Fprintf(stderr, "reasonrd: unknown format %q (text, json)\n", *format)
		return 2
	}
	rules, err := b.rules(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "reasonrd: %v\n", err)
		return 1
	}
	rows := featureRows(b.code(), rules, time.Now())
	if *format == "json" {
		out, _ := json.MarshalIndent(struct {
			Version  int          `json:"version"`
			Features []featureRow `json:"features"`
		}{1, rows}, "", "  ")
		fmt.Fprintln(stdout, string(out))
		return 0
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "FLAG\tDATE\tAGE\tROLLOUT\tDESCRIPTION")
	for _, row := range rows {
		rollout := strings.Join(row.Rollout, "; ")
		if rollout == "" {
			rollout = "off"
		}
		age := fmt.Sprintf("%dd", row.AgeDays)
		if row.Removed {
			row.Date, age, row.Doc = "—", "—", "removed from the code: switch it off (reasonrd feature off "+row.Name+")"
		}
		doc, _, _ := strings.Cut(row.Doc, "\n")
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", row.Name, row.Date, age, rollout, doc)
	}
	_ = w.Flush()
	return 0
}

// featureRows are the code flags and the flags left only in the store, by name.
func featureRows(code map[string]*codeFeature, rules []port.FeatureRule, now time.Time) []featureRow {
	byName := map[string]*featureRow{}
	for name, f := range code {
		row := &featureRow{Name: name, Date: f.Date, Doc: f.Doc, Models: f.Models, Rollout: []string{}}
		row.Context, _, _ = strings.Cut(name, ".")
		if row.Context == name {
			row.Context = ""
		}
		if d, err := time.Parse(time.DateOnly, f.Date); err == nil {
			row.AgeDays = int(now.Sub(d).Hours() / 24)
		}
		byName[name] = row
	}
	for _, rule := range rules {
		row := byName[rule.Feature]
		if row == nil {
			row = &featureRow{Name: rule.Feature, Removed: true, Rollout: []string{}}
			byName[rule.Feature] = row
		}
		row.Rollout = append(row.Rollout, rule.Scope.String())
	}
	out := make([]featureRow, 0, len(byName))
	for _, row := range byName {
		slices.Sort(row.Rollout)
		out = append(out, *row)
	}
	slices.SortFunc(out, func(a, b featureRow) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

func featureSwitch(ctx context.Context, on bool, args []string, stdout, stderr io.Writer) int {
	name, args := positional(args)
	verb := map[bool]string{true: "on", false: "off"}[on]
	c := newCommon("feature "+verb, stderr)
	actor := c.fs.String("actor", "", "an actor: Type:id, as in the Test actor= header")
	role := c.fs.String("role", "", "a role in a tenant (with --tenant)")
	tenant := c.fs.String("tenant", "", "a data tenant")
	all := c.fs.Bool("all", false, "the whole system")
	b, code := c.openFeatures(ctx, args, stderr)
	if code != 0 {
		return code
	}
	defer b.close()
	if name == "" && c.fs.NArg() > 0 {
		name = c.fs.Arg(0)
	}
	if name == "" {
		fmt.Fprintf(stderr, "reasonrd: feature %s <flag>\n", verb)
		return 2
	}
	code0 := b.code()
	full := resolveFeature(code0, name)
	switch {
	case full == "" && on:
		fmt.Fprintf(stderr, "reasonrd: the models have no feature flag %q (reasonrd feature list)\n", name)
		return 1
	case full == "":
		full = name // switch off a flag removed from the code
	}
	sc := port.FeatureScope{Tenant: port.Tenant(*tenant), Actor: *actor, Role: *role}
	if *role != "" && len(c.models) > 0 {
		sc.Role = c.models[0].model.Schema().Role(*role)
	}
	levels := 0
	for _, set := range []bool{*actor != "", *role != "", *tenant != "" && *role == "" && *actor == "", *all} {
		if set {
			levels++
		}
	}
	switch {
	case levels > 1:
		fmt.Fprintln(stderr, "reasonrd: set one level: --actor, --role with --tenant, --tenant or --all")
		return 2
	case levels == 0 && on:
		fmt.Fprintln(stderr, "reasonrd: set a level: --actor Type:id, --role role --tenant tenant, --tenant tenant or --all")
		return 2
	case *role != "" && *tenant == "":
		fmt.Fprintln(stderr, "reasonrd: --role is a role in a tenant: set --tenant")
		return 2
	}
	if on && *tenant != "" && c.cfg != nil {
		if _, ok := c.cfg.Tenants[*tenant]; !ok {
			fmt.Fprintf(stderr, "reasonrd: tenant %q is not in tenants (%s)\n", *tenant, c.configName())
			return 2
		}
	}
	if on && *actor != "" && len(c.models) > 0 {
		if err := c.checkActor(*actor, code0[full]); err != nil {
			fmt.Fprintf(stderr, "reasonrd: --actor: %v\n", err)
			return 2
		}
	}
	ch := port.FeatureChange{Feature: full, On: on, Scope: sc, Everywhere: !on && levels == 0, By: operator(), At: time.Now().UTC()}
	if err := b.change(ctx, ch); err != nil {
		fmt.Fprintf(stderr, "reasonrd: %v\n", err)
		return 1
	}
	what := sc.String()
	if ch.Everywhere {
		what = "every level"
	}
	fmt.Fprintf(stdout, "%s %s: %s\n", full, map[bool]string{true: "on", false: "off"}[on], what)
	return 0
}

// resolveFeature returns the full name of a code flag: the full one, or the short one if it is unique.
func resolveFeature(code map[string]*codeFeature, name string) string {
	s := &reasonr.Schema{}
	for _, f := range code {
		s.Features = append(s.Features, f.FeatureInfo)
	}
	return features.Resolve(s, name)
}

// checkActor checks the actor Type:id against the flag's models.
func (c *common) checkActor(actor string, f *codeFeature) error {
	typ, id, ok := strings.Cut(actor, ":")
	if !ok {
		return fmt.Errorf("expected Type:id")
	}
	for _, mi := range c.models {
		if f != nil && !slices.Contains(f.Models, mi.name) {
			continue
		}
		ti := mi.model.Schema().Lookup(typ)
		if ti == nil || ti.Kind != reasonr.TypeActor {
			return fmt.Errorf("%q is not an actor of model %s", typ, mi.name)
		}
		if _, err := codecs.JSONOf(mi.model.Schema(), codecs.Options{}).DecodeIDText(reasonr.NewArena(), ti.Ref, id); err != nil {
			return fmt.Errorf("actor id %s: %v", ti.Name, err)
		}
	}
	return nil
}

// operator is who changes the rollout: the OS user.
func operator() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return cmp.Or(os.Getenv("USER"), "reasonrd")
}

func featureLog(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	name, args := positional(args)
	c := newCommon("feature log", stderr)
	b, code := c.openFeatures(ctx, args, stderr)
	if code != 0 {
		return code
	}
	defer b.close()
	if name == "" && c.fs.NArg() > 0 {
		name = c.fs.Arg(0)
	}
	if name != "" {
		if full := resolveFeature(b.code(), name); full != "" {
			name = full
		}
	}
	log, err := b.log(ctx, name)
	if err != nil {
		fmt.Fprintf(stderr, "reasonrd: %v\n", err)
		return 1
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "WHEN\tWHO\tFLAG\tCHANGE")
	for _, ch := range log {
		what := "on " + ch.Scope.String()
		switch {
		case !ch.On && ch.Everywhere:
			what = "off at every level"
		case !ch.On:
			what = "off " + ch.Scope.String()
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", ch.At.Format(time.RFC3339), ch.By, ch.Feature, what)
	}
	_ = w.Flush()
	return 0
}
