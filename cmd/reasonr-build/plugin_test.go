package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/tycheg/reasonr-engine/internal/testmodels"
)

// needPlugins skips where Go plugins do not load.
func needPlugins(t *testing.T) {
	needGo(t)
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("Go plugins: Linux and macOS")
	}
}

// pluginManifest writes the manifest of a binary plugin with the test
// adapter adapter (testdata/<adapter>; the engine and the contract are the
// repository's, as in the host the test builds) and returns its path.
func pluginManifest(t *testing.T, adapter string) string {
	r := repo(t)
	path := filepath.Join(t.TempDir(), "plugin.yaml")
	src := `module: example.com/fakeplugin
binary: fake.so
plugins:
  - example.com/` + adapter + ` v0.1.0
replace:
  example.com/` + adapter + `: ` + r + `/cmd/reasonr-build/testdata/` + adapter + `
  github.com/tycheg/reasonr-engine: ` + r + `
  github.com/tycheg/reasonr-engine/port: ` + r + `/port
`
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// migrate runs the engine binary bin with a configuration that needs the
// test KMS and extra configuration lines.
func migrate(t *testing.T, bin, dir, extra string, args ...string) (string, error) {
	t.Helper()
	cfg := filepath.Join(dir, "engine.yaml")
	if err := os.WriteFile(cfg, []byte(extra+"stores:\n  main: { type: memory }\n  audit: { type: memory }\nstate: main\n"+
		"crypto:\n  kms: { type: fake }\n  forget_log: audit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := exec.Command(bin, append([]string{"migrate", "--config", cfg, "--model", testmodels.Dir("taxi")}, args...)...).CombinedOutput()
	return string(b), err
}

// TestPlugin: the test adapter built as a binary plugin for a host built
// from a manifest loads by plugins: of the configuration and by --plugin;
// a plugin built for another binary, and one whose adapter the binary
// already links, are refused.
func TestPlugin(t *testing.T) {
	needPlugins(t)
	r := repo(t)
	dir := t.TempDir()
	host := filepath.Join(dir, "acme-engine")
	var out, errb bytes.Buffer
	if code := run([]string{"-f", manifest(t, "  - example.com/fakekind v0.1.0\n",
		"  example.com/fakekind: "+r+"/cmd/reasonr-build/testdata/fakekind\n"), "-o", host}, &out, &errb); code != 0 {
		t.Fatalf("the host: %d\n%s", code, errb.String())
	}
	if b, err := migrate(t, host, dir, ""); err == nil || !strings.Contains(b, "fake") {
		t.Fatalf("without the plugin: %v\n%s", err, b)
	}
	so := filepath.Join(dir, "plugins", "fake.so")
	errb.Reset()
	if code := run([]string{"-f", pluginManifest(t, "fakeadapter"), "--host", host, "-o", so, "--keep"}, &out, &errb); code != 0 {
		t.Fatalf("the plugin: %d\n%s", code, errb.String())
	}
	_, kept, _ := strings.Cut(errb.String(), "build directory: ")
	kept, _, _ = strings.Cut(kept, "\n")
	t.Cleanup(func() { os.RemoveAll(kept) })
	if b, err := migrate(t, host, dir, "plugins: [plugins/fake.so]\n"); err != nil {
		t.Fatalf("plugins: in the configuration: %v\n%s", err, b)
	}
	if b, err := migrate(t, host, t.TempDir(), "", "--plugin", so); err != nil {
		t.Fatalf("--plugin: %v\n%s", err, b)
	}

	// Built without -trimpath, the plugin is not for this binary.
	other := filepath.Join(dir, "other.so")
	build := exec.Command("go", "build", "-buildmode=plugin", "-o", other, ".")
	build.Dir = kept
	build.Env = append(os.Environ(), "GOWORK=off")
	if b, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, b)
	}
	if b, err := migrate(t, host, t.TempDir(), "", "--plugin", other); err == nil ||
		!strings.Contains(b, "is built for another binary") || !strings.Contains(b, "-trimpath") {
		t.Fatalf("a plugin for another binary: %v\n%s", err, b)
	}

	// A binary that links the adapter: the plugin's copy of the same
	// package is the binary's (Go runs its init once), and another package
	// that registers the same name is refused.
	linked := filepath.Join(dir, "linked-engine")
	if code := run([]string{"-f", manifest(t, "  - example.com/fakekind v0.1.0\n  - example.com/fakeadapter v0.1.0\n",
		"  example.com/fakekind: "+r+"/cmd/reasonr-build/testdata/fakekind\n"+
			"  example.com/fakeadapter: "+r+"/cmd/reasonr-build/testdata/fakeadapter\n"), "-o", linked}, &out, &errb); code != 0 {
		t.Fatalf("the linked host: %d\n%s", code, errb.String())
	}
	same := filepath.Join(dir, "same.so")
	if code := run([]string{"-f", pluginManifest(t, "fakeadapter"), "--host", linked, "-o", same}, &out, &errb); code != 0 {
		t.Fatalf("the plugin for the linked host: %d\n%s", code, errb.String())
	}
	if b, err := migrate(t, linked, t.TempDir(), "", "--plugin", same); err != nil {
		t.Fatalf("the plugin of a linked package: %v\n%s", err, b)
	}
	dup := filepath.Join(dir, "dup.so")
	if code := run([]string{"-f", pluginManifest(t, "dupadapter"), "--host", linked, "-o", dup}, &out, &errb); code != 0 {
		t.Fatalf("the duplicate plugin: %d\n%s", code, errb.String())
	}
	if b, err := migrate(t, linked, t.TempDir(), "", "--plugin", dup); err == nil || !strings.Contains(b, `KMS "fake" registered twice`) {
		t.Fatalf("a duplicate registration: %v\n%s", err, b)
	}
}

// TestPluginManifest: engine and port of a plugin manifest must be the
// host's; a manifest of a binary still needs them.
func TestPluginManifest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugin.yaml")
	if err := os.WriteFile(path, []byte("plugins:\n  - example.com/x v1.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"-f", path, "-o", filepath.Join(t.TempDir(), "bin")}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "set engine and port") {
		t.Fatalf("a binary without engine: code %d\n%s", code, errb.String())
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	m := &Manifest{Engine: Requirement{"github.com/tycheg/reasonr-engine", "v9.9.9"}}
	if err := m.BuildPlugin(filepath.Join(t.TempDir(), "x.so"), exe, false, &errb); err == nil || !strings.Contains(err.Error(), "the manifest requires github.com/tycheg/reasonr-engine v9.9.9") {
		t.Fatalf("another engine: %v", err)
	}
}

// TestRaised: a module the plugin needs newer than the binary's is named
// with the modules that require that version.
func TestRaised(t *testing.T) {
	have := map[string]*debug.Module{"x": {Path: "x", Version: "v1.0.0"}, "y": {Path: "y", Version: "v1.0.0"}}
	linked := []byte("x v1.2.0\ny v1.0.0\nz v3.0.0\nx v1.2.0\n")
	graph := []byte("example.com/p example.com/lib@v1.0.0\nexample.com/lib@v1.0.0 x@v1.2.0\nexample.com/p x@v1.0.0\n")
	err := raised(linked, graph, have, func(string) string { return "" })
	if err == nil || !strings.Contains(err.Error(), "x v1.2.0, the binary has v1.0.0 (required by example.com/lib@v1.0.0)") || strings.Contains(err.Error(), "\n  y ") {
		t.Fatalf("%v", err)
	}
	if err := raised(linked, graph, have, func(p string) string { return p }); err != nil {
		t.Fatalf("replaced modules: %v", err)
	}
}
