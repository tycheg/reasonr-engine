// Package testmodels gives engine tests their models: the taxi and bank
// examples of the compiler module (compiler/examples, kept in sync with the
// RFC) and the engine's own test models (engine/testdata).
package testmodels

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// Root is the engine/testdata directory.
func Root() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata")
}

// examples is the examples directory of the compiler module the engine
// requires: the workspace directory here, the module cache in a copy.
var examples = sync.OnceValues(func() (string, error) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/tycheg/reasonr-compiler").Output()
	if err != nil {
		return "", err
	}
	return filepath.Join(strings.TrimSpace(string(out)), "examples"), nil
})

// Example is the project directory of the compiler example name (taxi,
// bank): reasonr.yaml and src.
func Example(name string) string {
	dir, err := examples()
	if err != nil {
		panic("testmodels: compiler module: " + err.Error())
	}
	return filepath.Join(dir, name)
}

// Dir is the sources of the model name: a compiler example (taxi, bank) or
// a model in engine/testdata.
func Dir(name string) string {
	if name == "taxi" || name == "bank" {
		return filepath.Join(Example(name), "src")
	}
	return filepath.Join(Root(), name)
}

// auth is the authentication module the taxi example requires.
const auth = "github.com/tycheg/reasonr-auth"

// Modules is the REASONR_MODULES value for copies of models outside
// engine/testdata: the taxi's authentication module is the replace
// directory of its reasonr.yaml or, without one, its version in the module
// cache (reasonr mod download in the example directory). A copy cannot use
// the example's own reasonr.yaml: replace is relative and reasonr.lock is
// not copied.
func Modules() string {
	dir := Example("taxi")
	data, err := os.ReadFile(filepath.Join(dir, "reasonr.yaml"))
	if err != nil {
		panic("testmodels: " + err.Error())
	}
	section, version := "", ""
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line[0] != ' ' {
			section, _, _ = strings.Cut(line, ":")
			continue
		}
		key, value, _ := strings.Cut(strings.TrimSpace(line), ":")
		if key != auth {
			continue
		}
		value = strings.TrimSpace(value)
		switch section {
		case "replace":
			if !filepath.IsAbs(value) {
				value = filepath.Join(dir, value)
			}
			return auth + "=" + value
		case "requires":
			version = value
		}
	}
	cache := os.Getenv("REASONR_MODCACHE")
	if cache == "" {
		d, err := os.UserCacheDir()
		if err != nil {
			panic("testmodels: module cache: " + err.Error())
		}
		cache = filepath.Join(d, "reasonr", "mod")
	}
	return auth + "=" + filepath.Join(cache, filepath.FromSlash(auth)+"@"+version)
}

// Published copies the model name into a temporary directory with an
// external API contract (api/contract.json, RFC §31.3.1) that publishes
// publish: outside test mode the engine exposes only what is published.
// The copy's modules come through REASONR_MODULES.
func Published(t testing.TB, name string, publish ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.CopyFS(dir, os.DirFS(Dir(name))); err != nil {
		t.Fatal(err)
	}
	// The example's own contract and released versions give way to publish.
	if err := os.RemoveAll(filepath.Join(dir, "api")); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]any{"version": 1, "publish": publish})
	if err := os.MkdirAll(filepath.Join(dir, "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "api", "contract.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REASONR_MODULES", Modules())
	return dir
}
