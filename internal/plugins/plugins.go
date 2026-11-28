// Package plugins loads binary plugins (plan 0024): Go plugins (go build
// -buildmode=plugin) whose packages register in init() through
// port/registry and kind, as the packages a manifest links do. A plugin
// loads only into a binary built with the same Go, the same build
// settings and the same version of every module both contain: the build
// information of the two is compared before the plugin is opened, and a
// difference names each module. A loaded plugin is never unloaded.
package plugins

import (
	"debug/buildinfo"
	"fmt"
	"os"
	"path/filepath"
	"plugin"
	"runtime/debug"
	"slices"
	"strings"
	"sync"

	"github.com/tycheg/reasonr-engine/port"
)

// Env is the variable that carries the plugins of --plugin to the
// processes the binary starts (reasonrd test, dev): a list separated by
// the path list separator.
const Env = "REASONR_PLUGINS"

var (
	mu     sync.Mutex
	loaded = map[string]bool{}
)

// Args takes the plugins out of the arguments of a command: --plugin path
// (repeated, also -plugin and =path), plugins: of the configuration that
// --config names (paths relative to its file) and those of Env. It loads
// them and returns the arguments without --plugin.
func Args(args []string) ([]string, error) {
	var paths, rest []string
	config := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		name, value, eq := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if !strings.HasPrefix(a, "-") {
			rest = append(rest, a)
			continue
		}
		switch name {
		case "plugin", "config":
			if !eq {
				if i+1 == len(args) {
					return nil, fmt.Errorf("flag needs an argument: -%s", name)
				}
				i++
				value = args[i]
			}
			if name == "plugin" {
				abs, err := filepath.Abs(value)
				if err != nil {
					return nil, err
				}
				paths = append(paths, abs)
				continue
			}
			config = value
			rest = append(rest, "--config", value)
		default:
			rest = append(rest, a)
		}
	}
	if env := os.Getenv(Env); env != "" {
		paths = append(filepath.SplitList(env), paths...)
	}
	if config != "" {
		fromConfig, err := Configured(config)
		if err != nil {
			return nil, err
		}
		paths = append(paths, fromConfig...)
	}
	if err := Load(paths); err != nil {
		return nil, err
	}
	return rest, nil
}

// Configured returns plugins: of the configuration file path, relative to
// its directory. A file that cannot be read or parsed has none here: the
// command reports it when it reads the configuration.
func Configured(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	root, err := port.ParseNode(data)
	if err != nil || root.Kind != port.NodeMap {
		return nil, nil
	}
	list, err := root.Strings("plugins")
	if err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	out := make([]string, 0, len(list))
	for _, p := range list {
		if !filepath.IsAbs(p) {
			p = filepath.Join(filepath.Dir(path), p)
		}
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, err
		}
		out = append(out, abs)
	}
	return out, nil
}

// Load loads the plugins paths that are not loaded yet and puts the ones
// given to it into Env for the processes the binary starts.
func Load(paths []string) error {
	mu.Lock()
	defer mu.Unlock()
	for _, p := range paths {
		if loaded[p] {
			continue
		}
		if err := open(p); err != nil {
			return err
		}
		loaded[p] = true
	}
	if len(loaded) > 0 {
		all := make([]string, 0, len(loaded))
		for p := range loaded {
			all = append(all, p)
		}
		slices.Sort(all)
		_ = os.Setenv(Env, strings.Join(all, string(os.PathListSeparator)))
	}
	return nil
}

// open checks the build information of the plugin path against the
// binary's and opens it; a registration the binary already has (a panic
// of init) is an error.
func open(path string) (err error) {
	host, ok := debug.ReadBuildInfo()
	if !ok {
		return fmt.Errorf("plugin %s: this binary has no build information", path)
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("plugin %s: %v", path, err)
	}
	if diff := Compare(host, info); len(diff) > 0 {
		return fmt.Errorf("plugin %s is built for another binary:\n  %s\nbuild it for this binary (reasonr-build --host) or take the one built for it",
			path, strings.Join(diff, "\n  "))
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("plugin %s: %v", path, r)
		}
	}()
	if _, err := plugin.Open(path); err != nil {
		if workspace(host) && strings.Contains(err.Error(), "different version of package") {
			return fmt.Errorf("plugin %s: %v: this binary is built in a Go workspace (go.work); a binary built without one (GOWORK=off) loads plugins", path, err)
		}
		return fmt.Errorf("plugin %s: %v", path, err)
	}
	return nil
}

// workspace reports whether the binary is built in a Go workspace: its
// modules there have no version and no replacement. Their packages differ
// from the same directories built outside the workspace.
func workspace(b *debug.BuildInfo) bool {
	for _, m := range b.Deps {
		if m.Version == "(devel)" && m.Replace == nil {
			return true
		}
	}
	return false
}

// settings are the build settings that must be the same in a binary and
// its plugins: they change the compiled packages.
var settings = []string{"-trimpath", "-tags", "-gcflags", "-asmflags", "GOOS", "GOARCH", "GOEXPERIMENT",
	"GOAMD64", "GOARM", "GOARM64", "GO386", "GOMIPS", "GOMIPS64", "GOPPC64", "GORISCV64", "GOWASM"}

// Compare returns the differences between the build information of a
// binary and of a plugin: the Go version, the build settings, cgo of the
// binary and every module both contain (the version and the sum, after
// replacements; a module from a directory or a workspace is left to
// plugin.Open, which compares the packages).
func Compare(host, plugin *debug.BuildInfo) []string {
	var diff []string
	if goVersion(host) != goVersion(plugin) {
		diff = append(diff, fmt.Sprintf("Go: %s in the plugin, %s in the binary", goVersion(plugin), goVersion(host)))
	}
	if setting(host, "CGO_ENABLED") != "1" {
		diff = append(diff, "the binary is built without cgo (CGO_ENABLED=1 is needed to load plugins)")
	}
	for _, key := range settings {
		if h, p := setting(host, key), setting(plugin, key); h != p {
			diff = append(diff, fmt.Sprintf("%s: %q in the plugin, %q in the binary", key, p, h))
		}
	}
	deps := map[string]string{}
	for _, m := range host.Deps {
		deps[m.Path] = module(m)
	}
	for _, m := range plugin.Deps {
		h, ok := deps[m.Path]
		if p := module(m); ok && h != "" && p != "" && h != p {
			diff = append(diff, fmt.Sprintf("%s: %s in the plugin, %s in the binary", m.Path, p, h))
		}
	}
	return diff
}

// goVersion is the Go release of a build without the experiments.
func goVersion(b *debug.BuildInfo) string {
	v, _, _ := strings.Cut(b.GoVersion, " ")
	return v
}

func setting(b *debug.BuildInfo, key string) string {
	for _, s := range b.Settings {
		if s.Key == key {
			if key == "-trimpath" && s.Value == "false" {
				return ""
			}
			return s.Value
		}
	}
	return ""
}

// module is a module's version and sum after its replacement; "" — a
// module from a directory or a workspace, which has no version to
// compare: opening the plugin compares its packages.
func module(m *debug.Module) string {
	if m.Replace != nil {
		m = m.Replace
	}
	if m.Version == "" || m.Version == "(devel)" {
		return ""
	}
	if m.Sum == "" {
		return m.Version
	}
	return m.Version + " (" + m.Sum + ")"
}
