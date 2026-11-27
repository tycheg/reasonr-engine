package main

import (
	"bytes"
	"go/format"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tycheg/reasonr-engine/internal/testmodels"
)

// repo is the repository root: the engine and the contract of the build
// are replaced by their directories, the compiler is the version the
// engine requires.
func repo(t *testing.T) string {
	t.Helper()
	r, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// manifest writes a manifest with the plugins and the replacements of the
// repository's modules and returns its path.
func manifest(t *testing.T, plugins, replace string) string {
	r := repo(t)
	path := filepath.Join(t.TempDir(), "components.yaml")
	src := `module: example.com/acme/engine
binary: acme-engine
engine: github.com/tycheg/reasonr-engine v0.1.0
port: github.com/tycheg/reasonr-engine/port v0.1.0
plugins:
` + plugins + `replace:
  github.com/tycheg/reasonr-engine: ` + r + `
  github.com/tycheg/reasonr-engine/port: ` + r + `/port
` + replace
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func needGo(t *testing.T) {
	if testing.Short() {
		t.Skip("building a binary")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go")
	}
}

// TestBuild: a binary with a third-party plugin that knows only the
// contract (testdata/fakeadapter: a KMS) runs and knows its type; the
// engine kind is the test double of testdata/fakekind.
func TestBuild(t *testing.T) {
	needGo(t)
	r := repo(t)
	m := manifest(t, "  - example.com/fakekind v0.1.0\n  - example.com/fakeadapter v0.1.0\n",
		"  example.com/fakekind: "+r+"/cmd/reasonr-build/testdata/fakekind\n"+
			"  example.com/fakeadapter: "+r+"/cmd/reasonr-build/testdata/fakeadapter\n")
	dir := t.TempDir()
	bin := filepath.Join(dir, "acme-engine")
	var out, errb bytes.Buffer
	if code := run([]string{"-f", m, "-o", bin}, &out, &errb); code != 0 {
		t.Fatalf("build: %d\n%s", code, errb.String())
	}
	cfg := filepath.Join(dir, "engine.yaml")
	if err := os.WriteFile(cfg, []byte("stores:\n  main: { type: memory }\n  audit: { type: memory }\nstate: main\n"+
		"crypto:\n  kms: { type: fake }\n  forget_log: audit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	model := testmodels.Dir("taxi")
	if b, err := exec.Command(bin, "migrate", "--config", cfg, "--model", model).CombinedOutput(); err != nil {
		t.Fatalf("the built server: %v\n%s", err, b)
	}
}

// TestReplaceFlag: --replace adds a replacement to the manifest's.
func TestReplaceFlag(t *testing.T) {
	needGo(t)
	m := manifest(t, "  - example.com/fakeadapter v0.1.0\n", "")
	var out, errb bytes.Buffer
	if code := run([]string{"-f", m, "-o", filepath.Join(t.TempDir(), "bin"),
		"--replace", "example.com/fakeadapter=" + filepath.Join("testdata", "fakeadapter")}, &out, &errb); code != 0 {
		t.Fatalf("code %d:\n%s", code, errb.String())
	}
	if code := run([]string{"-f", m, "--replace", "nothing"}, &out, &errb); code != 2 {
		t.Fatalf("a bad --replace: code %d", code)
	}
}

// TestContractVersions: a plugin that requires another version of the
// contract is refused, with the modules that require each version.
func TestContractVersions(t *testing.T) {
	needGo(t)
	r := repo(t)
	m := manifest(t, "  - example.com/newcontract v0.1.0\n",
		"  example.com/newcontract: "+r+"/cmd/reasonr-build/testdata/newcontract\n"+
			"  github.com/tycheg/reasonr-engine/port@v0.2.0: "+r+"/port\n")
	var out, errb bytes.Buffer
	if code := run([]string{"-f", m, "-o", filepath.Join(t.TempDir(), "bin")}, &out, &errb); code != 1 ||
		!strings.Contains(errb.String(), "different versions of the contract") || !strings.Contains(errb.String(), "example.com/newcontract@v0.1.0") || !strings.Contains(errb.String(), "v0.1.0: github.com/tycheg/reasonr-engine@v0.1.0") {
		t.Fatalf("code %d:\n%s", code, errb.String())
	}
}

// TestMain: --main writes the generated main.go, --check compares it.
func TestMain(t *testing.T) {
	m := manifest(t, "  - github.com/tycheg/reasonr-stateless/src v0.1.0\n  - example.com/b v1.0.0\n  - example.com/a v1.0.0\n", "")
	path := filepath.Join(t.TempDir(), "main.go")
	var out, errb bytes.Buffer
	if code := run([]string{"-f", m, "--main", path, "--check"}, &out, &errb); code != 1 {
		t.Fatalf("a missing file: code %d", code)
	}
	if code := run([]string{"-f", m, "--main", path}, &out, &errb); code != 0 {
		t.Fatalf("--main: %d\n%s", code, errb.String())
	}
	if code := run([]string{"-f", m, "--main", path, "--check"}, &out, &errb); code != 0 {
		t.Fatalf("--check: %d\n%s", code, errb.String())
	}
	src, _ := os.ReadFile(path)
	a, b := bytes.Index(src, []byte(`"example.com/a"`)), bytes.Index(src, []byte(`"example.com/b"`))
	if formatted, err := format.Source(src); err != nil || !bytes.Equal(formatted, src) {
		t.Fatalf("main.go is not gofmt-clean: %v\n%s", err, src)
	}
	if a < 0 || b < a || !bytes.Contains(src, []byte("server.Main")) {
		t.Fatalf("main.go:\n%s", src)
	}
}

func TestManifestErrors(t *testing.T) {
	dir := t.TempDir()
	for _, src := range []string{
		"engine: github.com/tycheg/reasonr-engine\n",
		"engine: github.com/tycheg/reasonr-engine v0.1.0\nport: p v1\nplugins: [x]\n",
		"adapters: [x]\n",
	} {
		path := filepath.Join(dir, "m.yaml")
		_ = os.WriteFile(path, []byte(src), 0o644)
		if _, err := Load(path); err == nil {
			t.Errorf("%q: no error", src)
		}
	}
}
