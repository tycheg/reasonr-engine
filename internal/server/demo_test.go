package server

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tycheg/reasonr-engine/internal/testmodels"
)

// TestDemo: reasonrd demo list shows the bank demo scenarios, demo load
// starts the engine from the configuration and fills a tenant with demo:
// true; a tenant without demo: true does not accept a scenario (RFC §27.5).
func TestDemo(t *testing.T) {
	model := testmodels.Dir("bank")
	var out, errOut bytes.Buffer
	if code := demoCommand(context.Background(), []string{"list", model}, &out, &errOut); code != 0 || strings.TrimSpace(out.String()) != "A month of transfers" {
		t.Fatalf("list: code %d: %s %s", code, out.String(), errOut.String())
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "kms.key")
	if err := os.WriteFile(key, []byte("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	cfg := filepath.Join(dir, "engine.yaml")
	yaml := fmt.Sprintf(`engine: stateless
stores:
  main: { type: memory }
state: main
ingress:
  - { type: http, listen: "%s" }
tenants:
  default: {}
  demo: { demo: true }
crypto:
  kms: { type: local, key_file: %s }
`, addr, key)
	if err := os.WriteFile(cfg, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	args := []string{"load", "A month of transfers", "--tenant", "demo", "--config", cfg, "--repeat", "3", "--parallel", "3", "--seed", "7", model}
	if code := demoCommand(context.Background(), args, &out, &errOut); code != 0 || !strings.Contains(out.String(), "3 runs in tenant demo") {
		t.Fatalf("load: code %d: %s\n%s", code, out.String(), errOut.String())
	}
	errOut.Reset()
	args = []string{"load", "A month of transfers", "--tenant", "default", "--config", cfg, model}
	if code := demoCommand(context.Background(), args, &out, &errOut); code != 2 || !strings.Contains(errOut.String(), "has no demo: true") {
		t.Fatalf("a tenant without demo: code %d: %s", code, errOut.String())
	}
}
