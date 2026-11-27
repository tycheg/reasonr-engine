package server

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tycheg/reasonr-engine/internal/testmodels"
)

func TestCheckCommand(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "engine.yaml")
	if err := os.WriteFile(cfg, []byte("stores:\n  main: { type: memory }\n  views: { type: memory }\nstate: main\nprojections:\n  OrderMart: views\nbus:\n  type: log\n"+
		"crypto:\n  kms: { type: memory }\n  forget_log: views\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	taxi := testmodels.Dir("taxi")
	var out, errb bytes.Buffer
	code := run([]string{"check", "--config", cfg, taxi}, &out, &errb)
	if code != 1 || !strings.Contains(out.String(), "C0013") || !strings.Contains(out.String(), "C0004") || strings.Contains(out.String(), "OrderMart") {
		t.Fatalf("code %d:\n%s%s", code, out.String(), errb.String())
	}
	out.Reset()
	code = run([]string{"check", "--config", cfg, "--format", "json", taxi}, &out, &errb)
	var res struct {
		Version     int
		Diagnostics []struct {
			Code, Severity, File string
			Range                struct{ Start struct{ Line int } }
		}
	}
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || code != 1 || res.Version != 1 || len(res.Diagnostics) != 3 ||
		res.Diagnostics[2].Range.Start.Line != 8 {
		t.Fatalf("json: %d %s %v", code, out.String(), err)
	}
}

// TestIntegrationsOpenAPI: reasonrd openapi --integrations prints the
// integration service contract of the production program — without fakes.
func TestIntegrationsOpenAPI(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"openapi", "--integrations", testmodels.Dir("taxi")}, &out, &errOut); code != 0 {
		t.Fatalf("code %d\n%s", code, errOut.String())
	}
	var doc struct {
		OpenAPI string                    `json:"openapi"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil || doc.OpenAPI != "3.1.0" || len(doc.Paths) != 2 ||
		doc.Paths["/requests/Matching/findDriver"]["post"] == nil || strings.Contains(out.String(), "IR sha256") || strings.Contains(out.String(), "Setup") {
		t.Fatalf("contract: %v\n%s", err, out.String())
	}
}

// TestUnknownEngineKind: an engine kind the binary does not link is a
// configuration error of every command, named with the kinds it has.
func TestUnknownEngineKind(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "engine.yaml")
	if err := os.WriteFile(cfg, []byte("engine: acme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	taxi := testmodels.Dir("taxi")
	for _, args := range [][]string{{"check", "--config", cfg, taxi}, {"migrate", "--config", cfg, "--model", taxi}} {
		var out, errb bytes.Buffer
		code := run(args, &out, &errb)
		if code == 0 || !strings.Contains(errb.String(), `engine "acme" is not linked into this binary (available: stateless)`) {
			t.Fatalf("%v: code %d\n%s%s", args, code, out.String(), errb.String())
		}
	}
}
