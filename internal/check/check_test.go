package check

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	reasonr "github.com/tycheg/reasonr-compiler"

	"github.com/tycheg/reasonr-engine/internal/config"
	"github.com/tycheg/reasonr-engine/internal/registry"
)

const model = `role admin

aggregate Box
  id uuid
  size int

  command(admin) Put(n int)
    if [Seen.Id(id)]
      seen()
      return
    put(n: n)
    Seen(id: Seen.Id(id), size: n)
    Uniq(id: Uniq.Id(id), size: n)
    Free(id: Free.Id(id), size: n)
    Hub.ping(boxId: id)

  event put(n int)
    size = n

  error seen

projection Seen
  id Box.Id
  size int

projection Uniq
  id Box.Id
  size int

  index bySize(size) unique sizeTaken

  error sizeTaken(size int)

projection Free
  id Box.Id
  size int

  query(admin) Get(boxId Box.Id) FreeRow
    let f = [Free.Id(boxId)]
    if f is none
      missing()
      return
    return FreeRow(size: f.size)

  error missing

value FreeRow
  size int

external Hub
  request ping(boxId Box.Id)
`

func program(t *testing.T) *reasonr.Model { return compile(t, model) }

func compile(t *testing.T, model string) *reasonr.Model {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "box.rr"), []byte(model), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := reasonr.Build(dir, reasonr.Options{Test: true})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCheck(t *testing.T) {
	p := program(t)
	cfg, err := config.Decode([]byte(`stores:
  main: { type: memory }
  views: { type: memory }
state: main
projections:
  default: views
integrations:
  Other: { type: http, url: "http://x" }
bus:
  type: log
`))
	if err != nil {
		t.Fatal(err)
	}
	got := Check(cfg, map[string]*reasonr.Model{"base": p}, Options{})
	var lines []string
	for _, pr := range got {
		lines = append(lines, pr.Text("engine.yaml"))
	}
	text := strings.Join(lines, "\n")
	for _, want := range []string{
		"engine.yaml:6: error C0001: projection Uniq with unique index bySize",
		"engine.yaml:6: error C0002: rows of projection Seen are checked by Box.Put",
		"engine.yaml:8: error C0005: the tenant models have no external system Other",
		"engine.yaml:8: warning C0004: external system Hub has no transport",
		"engine.yaml:8: error C0012: integrations.Other: HTTP without integrations.auth",
		"engine.yaml:10: error C0013: the bus reads events from the event store: set events",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in\n%s", want, text)
		}
	}
	if strings.Contains(text, "Free") || len(got) != 6 || !HasErrors(got) {
		t.Errorf("extra problems:\n%s", text)
	}
	ok, _ := config.Decode([]byte("projections:\n  Free: main\nintegrations:\n  auth: { key_env: K }\n  default: { type: http, url: \"http://x\" }\n"))
	if got := Check(ok, map[string]*reasonr.Model{"base": p}, Options{}); len(got) != 0 {
		t.Errorf("a valid configuration: %+v", got)
	}
}

const personalModel = `role admin

actor Person
  id uuid
  phone string? personal

  command(self) Register(newPhone string personal)
    registered(newPhone: newPhone)
    Card(id: Card.Id(id), person: id, phone: newPhone)

  event registered(newPhone string? personal)
    phone = newPhone

projection Card
  id Person.Id
  person Person.Id?
  phone string? personal(person)
`

func init() {
	for _, typ := range []string{"secure", "plain"} {
		traits := []registry.StoreTrait{registry.PersonalData}
		if typ == "plain" {
			traits = nil
		}
		registry.RegisterStore(typ, func(*config.Node, registry.Deps) (any, error) { return nil, nil }, traits...)
	}
}

// TestCheckPersonal: C0007–C0009 (RFC §20).
func TestCheckPersonal(t *testing.T) {
	models := map[string]*reasonr.Model{"base": compile(t, personalModel)}
	run := func(yaml string, opts Options) string {
		t.Helper()
		cfg, err := config.Decode([]byte(yaml))
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for _, pr := range Check(cfg, models, opts) {
			lines = append(lines, pr.Text("engine.yaml"))
		}
		return strings.Join(lines, "\n")
	}
	for _, c := range []struct {
		yaml string
		test bool
		want string
	}{
		{"stores:\n  main: { type: secure }\n", false,
			"engine.yaml: error C0007: the models have personal data, but crypto.kms is not set: the store cannot write it\n" +
				"engine.yaml: warning C0009: crypto.forget_log is not set: restoring the main database from a backup brings forgotten subjects back (reasonrd pii replay)"},
		{"stores:\n  main: { type: secure }\n", true, ""},
		{"stores:\n  main: { type: secure }\ncrypto:\n  kms: { type: memory }\n  forget_log: main\n", false,
			"engine.yaml:5: warning C0009: the forget journal is in the main store \"main\": a backup of the main database brings back both the forgotten subjects and a journal without them"},
		{"stores:\n  main: { type: secure }\n  views: { type: plain }\n  audit: { type: plain }\n  other: { type: unknown }\n" +
			"state: main\nprojections:\n  Card: views\nevents: other\ncrypto:\n  kms: { type: memory }\n  forget_log: audit\n", false,
			"engine.yaml:8: error C0008: projection Card: store \"views\" (type: plain) does not encrypt personal data"},
		{"stores:\n  main: { type: plain }\n  audit: { type: plain }\nstate: main\nevents: audit\ncrypto:\n  kms: { type: memory }\n  forget_log: audit\n", false,
			"engine.yaml:4: error C0008: state: store \"main\" (type: plain) does not encrypt personal data\n" +
				"engine.yaml:5: error C0008: event store: store \"audit\" (type: plain) does not encrypt personal data"},
	} {
		if got := run(c.yaml, Options{Test: c.test}); got != c.want {
			t.Errorf("%s\ngot:\n%s\nwant:\n%s", c.yaml, got, c.want)
		}
	}
}

// TestCheckIntegrations: C0010–C0012 (RFC §18).
func TestCheckIntegrations(t *testing.T) {
	roles := compile(t, "role matching\n\nexternal(matching) Matching\n  request find(n int)\n")
	plain := compile(t, "external Matching\n  request find(n int)\n")
	run := func(yaml string, p *reasonr.Model, opts Options) string {
		t.Helper()
		cfg, err := config.Decode([]byte(yaml))
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for _, pr := range Check(cfg, map[string]*reasonr.Model{"base": p}, opts) {
			lines = append(lines, pr.Code+":"+strconv.Itoa(pr.Line))
		}
		return strings.Join(lines, " ")
	}
	const any = "auth:\n  - type: jwt\n    secret: s\n    tenant: { claim: tid, any: \"*\" }\n"
	for _, c := range []struct {
		yaml string
		p    *reasonr.Model
		test bool
		want string
	}{
		{"integrations:\n  default: { type: fake }\n", roles, false, "C0010:2"},
		{"integrations:\n  default: { type: fake }\n", roles, true, ""},
		{"integrations:\n  default: { type: http, url: x }\n  Matching: { type: http, url: y }\n", roles, false, "C0012:2 C0012:3"},
		{"integrations:\n  default: { type: http, url: x }\n", roles, true, ""},
		{"integrations:\n  auth: { key_env: K }\n  default: { type: http, url: x }\n", roles, false, ""},
		{any, plain, false, "C0004:0 C0011:4"},
		{any, roles, false, "C0004:0"},
	} {
		if got := run(c.yaml, c.p, Options{Test: c.test}); got != c.want {
			t.Errorf("%s(test %v): %q, want %q", c.yaml, c.test, got, c.want)
		}
	}
}

// TestCheckUI: C0014–C0015 (RFC §22).
func TestCheckUI(t *testing.T) {
	p := compile(t, "aggregate A\n  id uuid\n")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	const jwt = "auth:\n  - type: jwt\n    secret: s\n"
	for _, c := range []struct{ yaml, want string }{
		{"ingress:\n  - type: http\n    ui: { dir: " + dir + " }\n", ""},
		{"ingress:\n  - type: http\n    ui: { dir: " + filepath.Join(dir, "nope") + " }\n", "C0014:3"},
		{"ingress:\n  - type: http\n    ui:\n      auth:\n        oidc: { issuer: https://idp }\n", "C0015:5"},
		{"ingress:\n  - type: http\n    ui:\n      auth:\n        oidc: { issuer: https://idp, client_id: ui }\n", "C0015:5"},
		{jwt + "ingress:\n  - type: http\n    ui:\n      auth:\n        oidc: { issuer: https://idp, client_id: ui }\n", ""},
	} {
		cfg, err := config.Decode([]byte(c.yaml))
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for _, pr := range Check(cfg, map[string]*reasonr.Model{"base": p}, Options{}) {
			lines = append(lines, pr.Code+":"+strconv.Itoa(pr.Line))
		}
		if got := strings.Join(lines, " "); got != c.want {
			t.Errorf("%s: %q, want %q", c.yaml, got, c.want)
		}
	}
}

// TestCheckPrimitives: C0016 — a primitive of a web app without an
// implementation in the runtime build.
func TestCheckPrimitives(t *testing.T) {
	// A web app draws the standard Card and its own Map; Chart is drawn
	// only by a mobile app (Model.Primitives("web") leaves it out).
	uses := map[string][]reasonr.PrimitiveUse{"base": {{Name: "Card", Std: true}, {Name: "Map"}}}
	dir := t.TempDir()
	write := func(name, s string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("index.html", "<html>")
	run := func(yaml string) string {
		t.Helper()
		cfg, err := config.Decode([]byte(yaml))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		c := &checker{cfg: cfg}
		c.primitives(uses)
		for _, pr := range c.out {
			out = append(out, pr.Code+":"+strconv.Itoa(pr.Line)+":"+pr.Message)
		}
		return strings.Join(out, "\n")
	}
	if got := run("ingress:\n  - type: http\n"); !strings.Contains(got, "C0016:2") || !strings.Contains(got, "Map") || strings.Contains(got, "Card") || strings.Contains(got, "Chart") {
		t.Errorf("the built-in build: %s", got)
	}
	ingress := "ingress:\n  - type: http\n    ui: { dir: " + dir + " }\n"
	if got := run(ingress); !strings.Contains(got, "no reasonr-ui.json") {
		t.Errorf("without a manifest: %s", got)
	}
	write("reasonr-ui.json", `{"std": true, "primitives": ["Map"]}`)
	if got := run(ingress); got != "" {
		t.Errorf("Map is implemented: %s", got)
	}
	write("reasonr-ui.json", `{"std": false, "primitives": ["Map"]}`)
	if got := run(ingress); !strings.Contains(got, "Card") {
		t.Errorf("without a directory: %s", got)
	}
}
