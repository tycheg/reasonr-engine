// Package golden compares a test result with a golden file. Running
// `go test ./... -update` rewrites the golden files.
package golden

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite golden files")

// Updating reports whether the tests run with the -update flag.
func Updating() bool { return *update }

// Check compares got with the content of the file path. With the -update
// flag the file is rewritten.
func Check(t testing.TB, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no golden file %s (run with -update): %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s: the result differs from the golden file (run with -update if the change is expected)\n%s", path, diff(string(want), string(got)))
	}
}

// diff is a simple line comparison: the first differences with context.
func diff(want, got string) string {
	w := strings.Split(want, "\n")
	g := strings.Split(got, "\n")
	var b strings.Builder
	shown := 0
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl == gl {
			continue
		}
		b.WriteString("line ")
		b.WriteString(strconv.Itoa(i + 1))
		b.WriteString(":\n  -")
		b.WriteString(wl)
		b.WriteString("\n  +")
		b.WriteString(gl)
		b.WriteString("\n")
		shown++
		if shown == 10 {
			b.WriteString("  ...\n")
			break
		}
	}
	return b.String()
}
