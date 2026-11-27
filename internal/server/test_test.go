package server

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tycheg/reasonr-engine/internal/testmodels"
)

// TestTestWeb: --driver web in a binary without the web driver is a usage
// error that names the module to link.
func TestTestWeb(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"test", "--driver", "web", "."}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "reasonr-web/driver") {
		t.Fatalf("code %d: %s", code, errb.String())
	}
}

func TestTest(t *testing.T) {
	taxi, bank := testmodels.Dir("taxi"), testmodels.Dir("bank")
	broken := filepath.Join("..", "testrun", "testdata", "broken")
	order := filepath.Join(taxi, "contexts", "Ordering", "order.test.rr")
	acme := filepath.Join(taxi, "extensions", "acme", "contexts", "Ordering", "order.test.rr")
	for _, c := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"test", "--check-determinism", taxi, bank}, 0, "[acme]\ttests: 36"},
		{[]string{"test", "-v", "--run", "unique", taxi}, 0, "error plateTaken"},
		{[]string{"test", broken}, 1, "failed: 9 of 11"},
		{[]string{"test", "--format", "json", "--run", "Passes", broken}, 0, `"passed": 1`},
		{[]string{"test", "--format", "xml", broken}, 2, ""},
		{[]string{"test", "--run", "(", broken}, 2, ""},
		{[]string{"test", "--extension", "nope", taxi}, 2, ""},
		// Without --url the server for api runs in this process; failures of
		// an api run carry the run number and the driver.
		{[]string{"test", "--driver", "api", bank}, 0, "tests: "},
		{[]string{"test", "--driver", "memory,api", broken}, 1, "run 2 (api): "},
		// file:line is the test containing the line; a tenant file — only its model.
		{[]string{"test", order + ":25", taxi}, 0, "tests: 1"},
		{[]string{"test", "-v", order + ":25"}, 0, "A busy driver"},
		{[]string{"test", acme}, 0, "[acme]\ttests: "},
		{[]string{"test", order + ":1"}, 2, ""},
		{[]string{"test", taxi + ":3"}, 2, ""},
		{[]string{"test", "--driver", "mobile", taxi}, 2, ""},
		{[]string{"test", "--driver", "memory,nope", taxi}, 2, ""},
		{[]string{"test", "--url", "http://x", taxi}, 2, ""},
	} {
		var out, errOut bytes.Buffer
		code := run(c.args, &out, &errOut)
		if code != c.code || !strings.Contains(out.String(), c.want) {
			t.Errorf("%v: code %d\n%s%s", c.args, code, out.String(), errOut.String())
		}
	}
	// An error in the model: tests do not run, code 1.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "m.rr"), []byte("aggregate A\n  id uuid\n  x Nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := run([]string{"test", dir}, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "Nope") {
		t.Errorf("a model with an error: code %d\n%s", code, errOut.String())
	}
}

// run is reasonrd with the arguments args, as from the command line.
func run(args []string, stdout, stderr io.Writer) int {
	return Run(context.Background(), args, stdout, stderr)
}
