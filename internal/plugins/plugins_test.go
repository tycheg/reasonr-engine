package plugins

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
)

// TestArgs: --plugin leaves the arguments, --config stays and its
// plugins: are relative to the file; a missing plugin is an error.
func TestArgs(t *testing.T) {
	t.Setenv(Env, "")
	dir := t.TempDir()
	cfg := filepath.Join(dir, "engine.yaml")
	if err := os.WriteFile(cfg, []byte("plugins: [plugins/a.so]\nstores: { main: { type: memory } }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Configured(cfg)
	if err != nil || len(got) != 1 || got[0] != filepath.Join(dir, "plugins", "a.so") {
		t.Fatalf("Configured: %v %v", got, err)
	}
	if got, err := Configured(filepath.Join(dir, "missing.yaml")); err != nil || got != nil {
		t.Fatalf("a missing configuration: %v %v", got, err)
	}
	rest, err := Args([]string{"serve", "--model", "m", "--test"})
	if err != nil || !slices.Equal(rest, []string{"serve", "--model", "m", "--test"}) {
		t.Fatalf("no plugins: %v %v", rest, err)
	}
	rest, err = Args([]string{"migrate", "-config=" + filepath.Join(dir, "none.yaml"), "--", "--plugin", "x"})
	if err != nil || !slices.Equal(rest, []string{"migrate", "--config", filepath.Join(dir, "none.yaml"), "--", "--plugin", "x"}) {
		t.Fatalf("after --: %v %v", rest, err)
	}
	if _, err := Args([]string{"serve", "--config", cfg}); err == nil || !strings.Contains(err.Error(), "a.so") {
		t.Fatalf("a missing plugin of the configuration: %v", err)
	}
	if _, err := Args([]string{"serve", "--plugin", filepath.Join(dir, "b.so")}); err == nil || !strings.Contains(err.Error(), "b.so") {
		t.Fatalf("a missing --plugin: %v", err)
	}
	if _, err := Args([]string{"serve", "--plugin"}); err == nil {
		t.Fatal("--plugin without a path")
	}
}

func TestCompare(t *testing.T) {
	mod := func(path, version string) *debug.Module {
		return &debug.Module{Path: path, Version: version, Sum: "h1:" + version}
	}
	host := &debug.BuildInfo{GoVersion: "go1.27.4", Settings: []debug.BuildSetting{{Key: "-trimpath", Value: "true"}, {Key: "CGO_ENABLED", Value: "1"},
		{Key: "GOOS", Value: "linux"}}, Deps: []*debug.Module{mod("a", "v1.0.0"), mod("b", "v1.0.0")}}
	same := &debug.BuildInfo{GoVersion: "go1.27.4 X:boringcrypto", Settings: host.Settings, Deps: []*debug.Module{mod("a", "v1.0.0"), mod("c", "v2.0.0")}}
	same.GoVersion = "go1.27.4"
	if diff := Compare(host, same); len(diff) != 0 {
		t.Fatalf("the same build: %v", diff)
	}
	other := &debug.BuildInfo{GoVersion: "go1.27.1", Settings: []debug.BuildSetting{{Key: "GOOS", Value: "linux"}},
		Deps: []*debug.Module{mod("a", "v1.1.0"), {Path: "b", Version: "v1.0.0", Replace: &debug.Module{Path: "../b", Version: "(devel)"}}}}
	diff := strings.Join(Compare(host, other), "\n")
	if strings.Contains(diff, "b:") {
		t.Errorf("a module from a directory is compared:\n%s", diff)
	}
	for _, want := range []string{"Go: go1.27.1 in the plugin, go1.27.4 in the binary", `-trimpath: "" in the plugin, "true" in the binary`,
		"a: v1.1.0 (h1:v1.1.0) in the plugin, v1.0.0 (h1:v1.0.0) in the binary"} {
		if !strings.Contains(diff, want) {
			t.Errorf("no %q in\n%s", want, diff)
		}
	}
	nocgo := &debug.BuildInfo{GoVersion: "go1.27.4", Settings: []debug.BuildSetting{{Key: "-trimpath", Value: "true"}, {Key: "GOOS", Value: "linux"}}}
	if diff := Compare(nocgo, same); len(diff) != 1 || !strings.Contains(diff[0], "without cgo") {
		t.Fatalf("a binary without cgo: %v", diff)
	}
}
