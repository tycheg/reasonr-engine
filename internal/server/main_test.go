package server

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/tycheg/reasonr-engine/internal/adapters/http/httpapi"
	"github.com/tycheg/reasonr-engine/internal/host"
	"github.com/tycheg/reasonr-engine/internal/testmodels"
	plugin "github.com/tycheg/reasonr-engine/port/registry"
)

// syncBuffer is the server log the test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestServer: reasonrd by engine.yaml with the taxi model (base and acme)
// serves the API and stops gracefully.
func TestServer(t *testing.T) {
	// The web runtime build comes from the binary (web/bundle); here a stub.
	plugin.RegisterUIBundle(fstest.MapFS{"index.html": {Data: []byte("<html>runtime</html>")}})
	t.Cleanup(func() { plugin.RegisterUIBundle(nil) })
	dir := t.TempDir()
	cfg := filepath.Join(dir, "engine.yaml")
	if err := os.WriteFile(cfg, []byte(`
stores:
  main: { type: memory }
notifications:
  delivery: [websocket, longpoll]
ingress:
  - { type: http, listen: "127.0.0.1:0", log: { requests: true } }
auth:
  - type: keys
    keys:
      - { key_env: REASONRD_TEST_KEY, tenant: acme-prod, roles: [admin] }
tenants:
  default: {}
  acme-prod: { extension: acme }
telemetry:
  logs: { format: json, level: info }
  metrics: { type: prometheus }
limits: { retries: 4 }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REASONRD_TEST_KEY", "k-1")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + ln.Addr().String()
	ctx, cancel := context.WithCancel(context.Background())
	var log syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- runWith(ctx, []string{"--config", cfg, "--model", testmodels.Dir("taxi"), "--test"}, os.Stdout, &log,
			func(e *host.Engine) { e.Listeners[0].(*httpapi.Listener).Listen = ln })
	}()
	do := func(method, path, who, body string) (int, string) {
		t.Helper()
		for range 100 {
			req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
			req.Header.Set("Authorization", who)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				time.Sleep(20 * time.Millisecond) // the server is still starting
				continue
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return resp.StatusCode, string(b)
		}
		t.Fatal("the server does not answer")
		return 0, ""
	}
	const d1 = "00000000-0000-4000-8000-0000000000d1"
	car := `{"driverName":"John","driverCar":{"plate":"A001AA77","model":"Kia Rio","carClass":"economy"}}`
	if code, body := do("POST", "/api/Driver/"+d1+"/Register", "Test role=admin", car); code != 200 || !strings.Contains(body, `"accepted"`) {
		t.Fatalf("Register: %d %s", code, body)
	}
	// The API key is tenant acme-prod with model acme: separate data, the plate is free.
	if code, body := do("POST", "/api/Driver/"+d1+"/Register", "ApiKey k-1", car); code != 200 {
		t.Fatalf("Register in acme-prod: %d %s", code, body)
	}
	if code, body := do("POST", "/api/Driver/00000000-0000-4000-8000-0000000000d2/Register", "ApiKey k-1", car); code != 422 || !strings.Contains(body, "plateTaken") {
		t.Fatalf("a repeat in acme-prod: %d %s", code, body)
	}
	if code, body := do("GET", "/api/Driver/MyState", "Test actor=Driver:"+d1, ""); code != 200 || !strings.Contains(body, `"status":"offline"`) {
		t.Fatalf("MyState: %d %s", code, body)
	}
	if code, body := do("GET", "/metrics", "", ""); code != 200 || !strings.Contains(body, "reasonr_commands_total") {
		t.Fatalf("metrics: %d", code)
	}
	if code, _ := do("GET", "/api/poll?wait=0s", "Test actor=Driver:"+d1, ""); code != 200 {
		t.Fatalf("long polling is on in the configuration: %d", code)
	}
	// The app API (RFC §22): test sign-in, the manifest, screen data, the
	// web runtime.
	if code, body := do("GET", "/app/_login", "", ""); code != 200 || body != `{"test":true}` {
		t.Fatalf("/app/_login: %d %s", code, body)
	}
	if code, body := do("GET", "/app/DriverApp", "Test actor=Driver:"+d1, ""); code != 200 || !strings.Contains(body, `"Shift"`) {
		t.Fatalf("the DriverApp manifest: %d %s", code, body)
	}
	if code, body := do("GET", "/app/DriverApp/Shift", "Test actor=Driver:"+d1, ""); code != 200 || !strings.Contains(body, `"status":"offline"`) {
		t.Fatalf("Shift data: %d %s", code, body)
	}
	if code, body := do("GET", "/ui/DriverApp/Shift", "", ""); code != 200 || !strings.Contains(body, "<html") {
		t.Fatalf("the web runtime: %d %.100s", code, body)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code %d\n%s", code, log.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the server did not stop")
	}
	for _, want := range []string{"TEST MODE", `"msg":"request"`, `"tenant":"acme-prod"`, "engine stopped"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("the log has no %q", want)
		}
	}
}

func TestFlags(t *testing.T) {
	var out bytes.Buffer
	for _, c := range []struct {
		args []string
		code int
		msg  string
	}{
		{nil, 2, "--model"},
		{[]string{"--model", "nope"}, 1, "nope"},
		{[]string{"--model", "x", "--config", "nope.yaml"}, 2, "nope.yaml"},
	} {
		out.Reset()
		if code := Run(context.Background(), c.args, os.Stdout, &out); code != c.code || !strings.Contains(out.String(), c.msg) {
			t.Errorf("%v: %d %s", c.args, code, out.String())
		}
	}
}
