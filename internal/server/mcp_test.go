package server

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/tycheg/reasonr-engine/internal/testmodels"
)

// TestMCPStdio is reasonrd mcp --stdio: an agent on a developer machine
// signs up a taxi customer and reads their profile with MCP tools.
func TestMCPStdio(t *testing.T) {
	t.Setenv("REASONR_MODULES", testmodels.Modules())
	const c1 = "00000000-0000-4000-8000-0000000000c1"
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"Ordering.Customer.SignUp","arguments":{"id":"` + c1 + `","phoneNumber":"+79990000000"}}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"Ordering.Customer.Profile","arguments":{}}}`,
	}, "\n") + "\n"
	var out, errOut bytes.Buffer
	code := mcpCommand(context.Background(), []string{"--stdio", "--as", "actor=Customer:" + c1, testmodels.Dir("taxi")},
		strings.NewReader(in), &out, &errOut)
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if code != 0 || len(lines) != 3 || !strings.Contains(lines[2], `"phone":"+79990000000"`) {
		t.Fatalf("code %d, responses:\n%s\n%s", code, out.String(), errOut.String())
	}
}
