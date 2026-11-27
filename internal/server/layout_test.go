package server

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestPublicPackages: the engine module has three public packages (plan
// 0022): kind, kind/kindtest and server (the contract of plugins, port, is
// a module of its own); every other package with Go code is under
// internal/ or is a command (cmd/), so that other modules see only the
// contracts.
func TestPublicPackages(t *testing.T) {
	public := []string{"kind", "kind/kindtest", "server"}
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if !d.IsDir() || rel == "." {
			return nil
		}
		switch d.Name() {
		case "internal", "cmd", "testdata", "docs", "node_modules":
			return filepath.SkipDir
		}
		if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
			return filepath.SkipDir
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return err
		}
		hasGo := slices.ContainsFunc(entries, func(e fs.DirEntry) bool {
			return strings.HasSuffix(e.Name(), ".go") && !strings.HasSuffix(e.Name(), "_test.go")
		})
		if hasGo && !slices.Contains(public, rel) {
			t.Errorf("%s: a public package outside %v; move it under internal/", rel, public)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
