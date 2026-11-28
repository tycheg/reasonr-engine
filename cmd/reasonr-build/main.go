// Command reasonr-build builds a ReasonR engine binary from a manifest
// (plan 0022, decision 7), like xcaddy and the OpenTelemetry Collector
// Builder: the engine, the contract of plugins and the plugins — Go
// packages that register in init() — with their versions:
//
//	module: example.com/acme/engine           # the module of the generated binary
//	binary: acme-engine                       # the binary name (default reasonrd)
//	engine: github.com/tycheg/reasonr-engine v0.2.0
//	port: github.com/tycheg/reasonr-engine/port v0.2.0
//	plugins:
//	  - github.com/tycheg/reasonr-stateless/src v0.2.0
//	  - github.com/tycheg/reasonr-postgres/src v0.2.0
//	  - example.com/acme/reasonr-oracle v1.2.0
//	replace:                                  # modules from local directories, relative to the manifest
//	  example.com/acme/reasonr-oracle: ./oracle
//
// It generates main.go and go.mod in a temporary module, resolves the
// versions with the go command and builds the binary. Every module of the
// build must require one version of the contract (port): the builder
// prints which module requires which otherwise.
//
//	reasonr-build [-f components.yaml] [-o binary] [--replace module=dir ...] [--keep]
//	reasonr-build [-f components.yaml] --main path [--check]
//	reasonr-build [-f plugin.yaml] --host path/to/reasonrd [-o plugin.so] [--replace module=dir ...] [--keep]
//
// --replace adds a replacement to those of the manifest (a build from
// sources: the modules of a checkout instead of their versions).
//
// With --main it writes only the generated main.go to path (a module that
// keeps its own go.mod, as a distribution does); --check fails if the file
// differs instead.
//
// With --host it builds the plugins of the manifest as one binary plugin
// (.so, plan 0024) for the binary at path: with its Go, build settings and
// the versions of its modules (plugin.go). engine and port may be left out
// of such a manifest; given, they must be the binary's.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tycheg/reasonr-engine/port"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

// goDirective is the go version of the generated go.mod: the Go of the
// official builds.
const goDirective = "1.27.4"

// Manifest is a build manifest.
type Manifest struct {
	Module, Binary string
	Engine, Port   Requirement
	Plugins        []Requirement
	// Replace maps a module (module or module@version) to a directory.
	Replace map[string]string
}

// Requirement is a module or package with its version.
type Requirement struct{ Path, Version string }

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("reasonr-build", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("f", "components.yaml", "the build manifest")
	output := fs.String("o", "", "the binary (default: binary of the manifest)")
	mainPath := fs.String("main", "", "write only the generated main.go to this path")
	check := fs.Bool("check", false, "with --main: fail if the file differs")
	keep := fs.Bool("keep", false, "keep the build directory (it is printed)")
	host := fs.String("host", "", "build a binary plugin (.so) for this engine binary")
	var replace []string
	fs.Func("replace", "module=dir: replace a module with a directory (repeated)", func(v string) error {
		if mod, dir, ok := strings.Cut(v, "="); !ok || mod == "" || dir == "" {
			return fmt.Errorf("expected module=dir")
		}
		replace = append(replace, v)
		return nil
	})
	if err := fs.Parse(args); err != nil {
		return 2
	}
	m, err := Load(*file)
	if err != nil {
		fmt.Fprintf(stderr, "reasonr-build: %v\n", err)
		return 2
	}
	for _, r := range replace {
		mod, dir, _ := strings.Cut(r, "=")
		if m.Replace[mod], err = filepath.Abs(dir); err != nil {
			fmt.Fprintf(stderr, "reasonr-build: %v\n", err)
			return 2
		}
	}
	if *host == "" && (m.Engine.Path == "" || m.Port.Path == "") {
		fmt.Fprintf(stderr, "reasonr-build: %s: set engine and port\n", *file)
		return 2
	}
	source := m.Main()
	if *mainPath != "" {
		return writeMain(*mainPath, source, *check, stderr)
	}
	out := *output
	if out == "" {
		out = m.Binary
	}
	if out, err = filepath.Abs(out); err != nil {
		fmt.Fprintf(stderr, "reasonr-build: %v\n", err)
		return 2
	}
	build := m.Build
	if *host != "" {
		build = func(out string, keep bool, log io.Writer) error { return m.BuildPlugin(out, *host, keep, log) }
	}
	if err := build(out, *keep, stderr); err != nil {
		fmt.Fprintf(stderr, "reasonr-build: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, out)
	return 0
}

func writeMain(path string, source []byte, check bool, stderr io.Writer) int {
	if check {
		have, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(have, source) {
			fmt.Fprintf(stderr, "reasonr-build: %s does not match the manifest: run reasonr-build --main %s\n", path, path)
			return 1
		}
		return 0
	}
	if err := os.WriteFile(path, source, 0o644); err != nil {
		fmt.Fprintf(stderr, "reasonr-build: %v\n", err)
		return 1
	}
	return 0
}

// Load reads a manifest; replace paths become absolute.
func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	root, err := port.ParseNode(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	if err := root.Only("module", "binary", "engine", "port", "plugins", "replace"); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	m := &Manifest{Replace: map[string]string{}}
	if m.Binary, err = root.String("binary", "reasonrd"); err != nil {
		return nil, err
	}
	if m.Module, err = root.String("module", "reasonr.local/"+m.Binary); err != nil {
		return nil, err
	}
	for _, f := range []struct {
		key string
		to  *Requirement
	}{{"engine", &m.Engine}, {"port", &m.Port}} {
		v, err := root.String(f.key, "")
		if err != nil {
			return nil, err
		}
		if v == "" {
			continue // required unless the manifest is of a binary plugin (--host)
		}
		if *f.to, err = requirement(v); err != nil {
			return nil, fmt.Errorf("%s: %s: %v", path, f.key, err)
		}
	}
	plugins, err := root.Strings("plugins")
	if err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	for _, p := range plugins {
		r, err := requirement(p)
		if err != nil {
			return nil, fmt.Errorf("%s: plugins: %v", path, err)
		}
		m.Plugins = append(m.Plugins, r)
	}
	if r := root.Get("replace"); r != nil {
		base := filepath.Dir(path)
		for i, mod := range r.Keys {
			dir := r.Values[i].Value
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(base, dir)
			}
			if m.Replace[mod], err = filepath.Abs(dir); err != nil {
				return nil, err
			}
		}
	}
	return m, nil
}

// requirement parses "path version".
func requirement(s string) (Requirement, error) {
	f := strings.Fields(s)
	if len(f) != 2 || !strings.HasPrefix(f[1], "v") {
		return Requirement{}, fmt.Errorf("%q: expected \"path version\"", s)
	}
	return Requirement{Path: f[0], Version: f[1]}, nil
}

// Main is the generated main.go.
func (m *Manifest) Main() []byte {
	var b bytes.Buffer
	b.WriteString("// Code generated by reasonr-build from the manifest. DO NOT EDIT.\n\n")
	b.WriteString("// Command " + m.Binary + " is a ReasonR engine with the plugins of its manifest.\npackage main\n\n")
	b.WriteString("import (\n\t\"os\"\n\n")
	const server = "github.com/tycheg/reasonr-engine/server"
	imports := []string{server}
	for _, p := range m.Plugins {
		imports = append(imports, p.Path)
	}
	sort.Strings(imports) // as gofmt orders them
	for _, p := range imports {
		if p == server {
			fmt.Fprintf(&b, "\t%q\n", p)
		} else {
			fmt.Fprintf(&b, "\t_ %q\n", p)
		}
	}
	b.WriteString(")\n\nfunc main() { os.Exit(server.Main(os.Args[1:])) }\n")
	return b.Bytes()
}

// module returns the replaced module of a package; "" — not replaced.
func (m *Manifest) module(pkg string) string {
	best := ""
	for key := range m.Replace {
		mod, _, _ := strings.Cut(key, "@")
		if (pkg == mod || strings.HasPrefix(pkg, mod+"/")) && len(mod) > len(best) {
			best = mod
		}
	}
	return best
}

// GoMod is the go.mod of the build module before the versions are
// resolved: the engine, the contract, the replaced modules of plugins and
// the replacements.
func (m *Manifest) GoMod() []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "module %s\n\ngo %s\n\nrequire (\n", m.Module, goDirective)
	reqs := map[string]string{m.Engine.Path: m.Engine.Version, m.Port.Path: m.Port.Version}
	for _, p := range m.Plugins {
		if mod := m.module(p.Path); mod != "" {
			reqs[mod] = p.Version
		}
	}
	paths := make([]string, 0, len(reqs))
	for p := range reqs {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		fmt.Fprintf(&b, "\t%s %s\n", p, reqs[p])
	}
	b.WriteString(")\n")
	keys := make([]string, 0, len(m.Replace))
	for k := range m.Replace {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		b.WriteString("\nreplace (\n")
		for _, k := range keys {
			fmt.Fprintf(&b, "\t%s => %s\n", strings.Replace(k, "@", " ", 1), m.Replace[k])
		}
		b.WriteString(")\n")
	}
	return b.Bytes()
}

// Build generates the build module, resolves the versions and builds the
// binary out.
func (m *Manifest) Build(out string, keep bool, log io.Writer) error {
	dir, err := os.MkdirTemp("", "reasonr-build")
	if err != nil {
		return err
	}
	if keep {
		fmt.Fprintln(log, "build directory:", dir)
	} else {
		defer os.RemoveAll(dir)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), m.Main(), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), m.GoMod(), 0o644); err != nil {
		return err
	}
	gocmd := func(args ...string) ([]byte, error) {
		cmd := exec.Command("go", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOWORK=off")
		var stdout bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, log
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("go %s: %v", strings.Join(args, " "), err)
		}
		return stdout.Bytes(), nil
	}
	for _, p := range m.Plugins {
		if m.module(p.Path) == "" {
			if _, err := gocmd("get", p.Path+"@"+p.Version); err != nil {
				return err
			}
		}
	}
	if _, err := gocmd("mod", "tidy"); err != nil {
		return err
	}
	graph, err := gocmd("mod", "graph")
	if err != nil {
		return err
	}
	if err := oneVersion(graph, m.Port.Path); err != nil {
		return err
	}
	_, err = gocmd("build", "-trimpath", "-o", out, ".")
	return err
}

// oneVersion checks that the modules of the graph (go mod graph) require
// one version of the module mod.
func oneVersion(graph []byte, mod string) error {
	by := map[string][]string{} // version → requirers
	for _, line := range strings.Split(strings.TrimSpace(string(graph)), "\n") {
		from, to, ok := strings.Cut(line, " ")
		path, version, _ := strings.Cut(to, "@")
		if ok && path == mod {
			by[version] = append(by[version], from)
		}
	}
	if len(by) <= 1 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "the modules require different versions of the contract %s:", mod)
	versions := make([]string, 0, len(by))
	for v := range by {
		versions = append(versions, v)
	}
	sort.Strings(versions)
	for _, v := range versions {
		sort.Strings(by[v])
		fmt.Fprintf(&b, "\n  %s: %s", v, strings.Join(by[v], ", "))
	}
	return fmt.Errorf("%s", b.String())
}
