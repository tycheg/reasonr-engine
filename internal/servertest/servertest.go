// Package servertest runs the reasonrd server in tests: start it on a free
// port, call its API, run a model's acceptance tests through the API. It
// takes the server entry point as an argument, so both the server's own
// tests and tests of binaries built on it (adapters, engine kinds) use it.
package servertest

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"

	"github.com/tycheg/reasonr-engine/internal/adapters/http/httpapi"
	"github.com/tycheg/reasonr-engine/internal/host"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/internal/testrun"
)

// RunFunc is the server entry point with a hook: ready gets the built
// engine before the roles start.
type RunFunc func(ctx context.Context, args []string, stdout, stderr io.Writer, ready func(*host.Engine)) int

// Serve is the server's RunFunc; the server package sets it (a binary or a
// test that links the server has it).
var Serve RunFunc

// LoadPrograms compiles the models of the data tenants like the server
// does at start; the server package sets it.
var LoadPrograms func(dir string, tenants map[string]string, test bool) (*kernel.Programs, error)

// Start runs the server with args on a free local port (the first ingress
// listener) and returns its base URL and the stop function, which checks
// the exit code. The server log is printed when the test fails.
func Start(t testing.TB, run RunFunc, args ...string) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	log := &Buffer{}
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, args, log, log, func(e *host.Engine) { e.Listeners[0].(*httpapi.Listener).Listen = ln })
	}()
	t.Cleanup(func() {
		if t.Failed() {
			t.Log(log.String())
		}
	})
	return "http://" + ln.Addr().String(), func() {
		cancel()
		select {
		case code := <-done:
			if code != 0 {
				t.Fatalf("exit code %d\n%s", code, log.String())
			}
		case <-time.After(15 * time.Second):
			t.Fatal("the server did not stop")
		}
	}
}

// Call sends a request with the Authorization header who and returns the
// status and the body; it retries while the server is not listening yet.
func Call(t testing.TB, base, method, path, who, body string) (int, string) {
	t.Helper()
	for range 200 {
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("Authorization", who)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}
	t.Fatal("the server does not answer")
	return 0, ""
}

// Scenarios runs the acceptance tests of the model in dir (extension
// tenant, "" for the base model) through the API of the server at base;
// each test runs in an ephemeral tenant.
func Scenarios(t testing.TB, base, dir, tenant string) {
	t.Helper()
	bc, err := reasonr.Build(dir, reasonr.Options{Extension: tenant, Test: true})
	if err != nil {
		t.Fatal(err)
	}
	model := tenant
	if model == "" {
		model = "base"
	}
	rep, err := testrun.Run(context.Background(), bc, testrun.Options{Parallel: 4,
		Drivers: []testrun.Driver{{Name: "api", New: func(ctx context.Context, _ int) (reasonr.Driver, func(), error) {
			d, err := testrun.NewHTTPDriver(ctx, base, model, bc.Schema())
			if err != nil {
				return nil, nil, err
			}
			return d, func() {
				if err := d.Close(context.Background()); err != nil {
					t.Error(err)
				}
			}, nil
		}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Tests) == 0 || rep.Failed() > 0 {
		var b bytes.Buffer
		_ = testrun.WriteText(&b, rep, true)
		t.Fatalf("%s %s: %d tests, %d failed:\n%s", dir, tenant, len(rep.Tests), rep.Failed(), b.String())
	}
}

// Buffer is an io.Writer safe for concurrent use: the server log.
type Buffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *Buffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *Buffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
