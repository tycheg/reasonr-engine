package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tycheg/reasonr-engine/internal/adapters/auth"
	"github.com/tycheg/reasonr-engine/internal/bus"
	"github.com/tycheg/reasonr-engine/internal/config"
	"github.com/tycheg/reasonr-engine/internal/registry"
	"github.com/tycheg/reasonr-engine/internal/servertest"
	"github.com/tycheg/reasonr-engine/internal/testmodels"
	"github.com/tycheg/reasonr-engine/port"
)

// platformBus is a platform's own bus adapter, registered the way a
// platform build registers its adapters: it encodes every message as a
// CloudEvents webhook would and keeps the attributes and the body.
var platformBus = &capturedBus{}

func init() {
	registry.RegisterPublisher("platform-test", func(n *config.Node, _ registry.Deps) (port.Publisher, error) {
		if err := n.Only("type", "notifications"); err != nil {
			return nil, err
		}
		return platformBus, nil
	})
}

type capturedBus struct {
	mu     sync.Mutex
	events []map[string]string // ce-* attributes and "data"
}

func (b *capturedBus) Publish(_ context.Context, msgs []port.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i := range msgs {
		e := map[string]string{"data": string(msgs[i].Data)}
		for _, a := range bus.Headers(&msgs[i], "ce-") {
			e[a.Name] = a.Value
		}
		b.events = append(b.events, e)
	}
	return nil
}

// event returns the first published message of type typ.
func (b *capturedBus) event(typ string) map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range b.events {
		if e["ce-type"] == typ {
			return e
		}
	}
	return nil
}

// TestPlatform: the taxi model in production the way a platform runs it,
// in one process and in memory — the platform's own bus adapter, its own
// UI build, an integration service by the contract instead of fakes, sign-in
// by JWT. engine check accepts the configuration; a ride goes from the
// order through Matching and Payments to "paid"; the events reach the own
// bus as CloudEvents with personal arguments null; the server serves the
// own UI build and the app API to a signed-in customer. The pilot platform
// checks the same on its own binary and PostgreSQL.
func TestPlatform(t *testing.T) {
	const (
		secret = "idp-secret"
		c1     = "00000000-0000-4000-8000-0000000000c1"
		d1     = "00000000-0000-4000-8000-0000000000d1"
		o1     = "00000000-0000-4000-8000-0000000000a1"
	)
	t.Setenv("REASONR_MODULES", testmodels.Modules())
	master := make([]byte, 32)
	_, _ = rand.Read(master)
	t.Setenv("PLATFORM_MASTER_KEY", base64.StdEncoding.EncodeToString(master))
	sign := func(claims map[string]any) string {
		if claims["tid"] == nil {
			claims["tid"] = "default"
		}
		tok, err := auth.Sign(claims, []byte(secret))
		if err != nil {
			t.Fatal(err)
		}
		return "Bearer " + tok
	}

	var engine string
	svc := &contractService{t: t, engine: &engine, replies: map[string]func(map[string]any) (string, string){
		"Matching/findDriver": func(body map[string]any) (string, string) {
			return "/api/Trip/" + body["tripId"].(string) + "/DriverFound", `{"driverId":"` + d1 + `"}`
		},
		"Payments/charge": func(body map[string]any) (string, string) {
			return "/api/Order/" + body["paymentId"].(string) + "/PaymentCaptured", `{"transactionId":"tx-1"}`
		},
	}, tokens: map[string]string{
		"Matching": sign(map[string]any{"tid": "*", "roles": []any{"matching"}}),
		"Payments": sign(map[string]any{"tid": "*", "roles": []any{"payments"}}),
	}}
	integrations := httptest.NewServer(svc)
	defer integrations.Close()

	dir := t.TempDir()
	ui := filepath.Join(dir, "ui")
	if err := os.MkdirAll(ui, 0o755); err != nil {
		t.Fatal(err)
	}
	const index = "<!doctype html><title>the platform's own UI build</title>"
	if err := os.WriteFile(filepath.Join(ui, "index.html"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	// The build manifest of @tycheg/reasonr-vite: the standard catalog and
	// the model's own primitive.
	if err := os.WriteFile(filepath.Join(ui, "reasonr-ui.json"), []byte(`{"std": true, "primitives": ["Tag"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := filepath.Join(dir, "engine.yaml")
	if err := os.WriteFile(cfg, []byte(fmt.Sprintf(`stores:
  main: { type: memory }
  audit: { type: memory }
state: main
events: audit
bus: { type: platform-test }
integrations:
  auth: { key_file: %q, kid: k1, issuer: reasonr-platform }
  workers: 4
  retry: { initial: 50ms, multiplier: 2, max_interval: 1s, max_elapsed: 1m }
  default: { type: http, url: %q, timeout: 5s }
ingress:
  - { type: http, listen: ":0", ui: { dir: %q } }
auth:
  - type: jwt
    secret: %q
    tenant: { claim: tid, any: "*" }
    actor:  { claim: sub, type: { claim: actor_type } }
    roles:  { claim: roles }
crypto:
  kms: { type: local, key_env: PLATFORM_MASTER_KEY }
  forget_log: audit
`, signingKey(t), integrations.URL, ui, secret)), 0o644); err != nil {
		t.Fatal(err)
	}
	model := testmodels.Published(t, "taxi", "Ordering.Customer.*", "Ordering.Order.*", "Dispatch.Driver.*")

	var check servertest.Buffer
	if code := Run(context.Background(), []string{"check", "--config", cfg, model}, os.Stdout, &check); code != 0 || strings.Contains(check.String(), "warning") {
		t.Fatalf("check: %d\n%s", code, check.String())
	}
	base, stop := servertest.Start(t, runWith, "--config", cfg, "--model", model)
	defer stop()
	engine = base

	customer := sign(map[string]any{"sub": c1, "actor_type": "Customer"})
	driver := sign(map[string]any{"sub": d1, "actor_type": "Driver"})
	admin := sign(map[string]any{"roles": []any{"admin"}})
	must := func(code int, body string) string {
		t.Helper()
		if code != http.StatusOK {
			t.Fatalf("%d %s", code, body)
		}
		return body
	}
	waitStatus := func(status string) {
		t.Helper()
		for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			body := must(servertest.Call(t, base, "GET", "/api/Order/Card?orderId="+o1, customer, ``))
			if strings.Contains(body, `"status":"`+status+`"`) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("order: want %s, %s", status, body)
			}
		}
	}
	must(servertest.Call(t, base, "POST", "/api/Driver/"+d1+"/Register", admin,
		`{"driverName":"John","driverCar":{"plate":"A001AA77","model":"Kia Rio","carClass":"economy"}}`))
	must(servertest.Call(t, base, "POST", "/api/Driver/"+d1+"/GoOnline", driver, ``))
	must(servertest.Call(t, base, "POST", "/api/Customer/"+c1+"/SignUp", customer, `{"phoneNumber":"+79990000000"}`))
	must(servertest.Call(t, base, "POST", "/api/Customer/"+c1+"/OrderTaxi", customer,
		`{"orderId":"`+o1+`","orderRoute":{"from":{"lat":55.75,"lon":37.62},"to":{"lat":55.8,"lon":37.6},"distanceKm":3},"requestedClass":"economy"}`))
	waitStatus("offered") // Matching answered DriverFound
	must(servertest.Call(t, base, "POST", "/api/Driver/"+d1+"/Accept", driver, ``))
	must(servertest.Call(t, base, "POST", "/api/Driver/"+d1+"/FinishRide", driver, ``))
	waitStatus("paid") // Payments answered PaymentCaptured
	svc.replied.Wait()

	// Production mode: no test API, no fakes; an unknown token is refused.
	if code, body := servertest.Call(t, base, "POST", "/_test/tenants", customer, `{}`); code == http.StatusOK {
		t.Fatalf("the test API in production: %d %s", code, body)
	}
	if code, _ := servertest.Call(t, base, "GET", "/api/Order/Card?orderId="+o1, "Bearer x.y.z", ``); code != http.StatusUnauthorized {
		t.Fatalf("a bad token: %d", code)
	}

	// The own UI build and the app API for a signed-in customer.
	if code, body := servertest.Call(t, base, "GET", "/ui/", "", ``); code != http.StatusOK || body != index {
		t.Fatalf("/ui/: %d %q", code, body)
	}
	if code, body := servertest.Call(t, base, "GET", "/app/Taxi", customer, ``); code != http.StatusOK || !strings.Contains(body, `"screens"`) {
		t.Fatalf("/app/Taxi: %d %s", code, body)
	}

	// The events reach the own bus as CloudEvents; personal arguments are null.
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if platformBus.event("Ordering.Order.paymentReceived") != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no payment event on the bus: %v", platformBus.events)
		}
	}
	signed := platformBus.event("Ordering.Customer.signedUp")
	if signed == nil || signed["ce-subject"] != c1 || signed["ce-specversion"] != "1.0" || !strings.Contains(signed["data"], `"phoneNumber":null`) {
		t.Fatalf("Customer.signedUp on the bus: %v", signed)
	}
}

// contractService is an integration service by the contract of RFC §18: it
// checks the engine's JWT by the engine's JWKS, accepts the request and
// answers later with a command on behalf of the system.
type contractService struct {
	t       *testing.T
	engine  *string
	replies map[string]func(body map[string]any) (path, args string) // by "System/request"
	tokens  map[string]string                                        // the system's Authorization

	mu       sync.Mutex
	verifier map[string]*auth.JWT
	replied  sync.WaitGroup
}

func (s *contractService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, ok := strings.CutPrefix(r.URL.Path, "/requests/")
	reply := s.replies[name]
	if !ok || reply == nil {
		http.NotFound(w, r)
		return
	}
	system, _, _ := strings.Cut(name, "/")
	s.mu.Lock()
	if s.verifier == nil {
		s.verifier = map[string]*auth.JWT{}
	}
	v := s.verifier[system]
	if v == nil {
		v = &auth.JWT{Keys: &auth.JWKS{URL: *s.engine + "/.well-known/jwks.json"}, Issuer: "reasonr-platform", Audience: system}
		s.verifier[system] = v
	}
	s.mu.Unlock()
	tenant, key := r.Header.Get("X-Reasonr-Tenant"), r.Header.Get("Idempotency-Key")
	claims, err := v.Verify(r.Context(), auth.ParseAuthorization(r.Header.Get("Authorization")).Token)
	if err != nil || claims["tenant"] != tenant || claims["jti"] != key {
		s.t.Errorf("%s: the engine's JWT %v, %v; tenant %q, key %q", name, claims, err, tenant, key)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body map[string]any
	if b, _ := io.ReadAll(r.Body); json.Unmarshal(b, &body) != nil {
		s.t.Errorf("%s: body %s", name, b)
	}
	w.WriteHeader(http.StatusAccepted)
	path, args := reply(body)
	s.replied.Add(1)
	go func() { // the system answers later, with its own call of the engine API
		defer s.replied.Done()
		req, _ := http.NewRequest(http.MethodPost, *s.engine+path, strings.NewReader(args))
		req.Header.Set("Authorization", s.tokens[system])
		req.Header.Set("X-Reasonr-Tenant", tenant)
		req.Header.Set("Idempotency-Key", key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			s.t.Errorf("%s: answer %s: %v", name, path, err)
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			out, _ := io.ReadAll(resp.Body)
			s.t.Errorf("%s: answer %s: %d %s", name, path, resp.StatusCode, out)
		}
	}()
}

// signingKey writes the engine's JWT signing key (EC P-256, PKCS #8) to a
// temporary file and returns its path.
func signingKey(t *testing.T) string {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	path := filepath.Join(t.TempDir(), "engine.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
