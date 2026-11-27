package httpapi_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/auth"
	"github.com/tycheg/reasonr-engine/internal/adapters/http/httpapi"
	"github.com/tycheg/reasonr-engine/internal/adapters/memory"
	"github.com/tycheg/reasonr-engine/internal/adapters/telemetry"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/internal/testmodels"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// The taxi API through httptest on a kernel with memory adapters.

const (
	c1 = "00000000-0000-4000-8000-0000000000c1"
	c2 = "00000000-0000-4000-8000-0000000000c2"
	d1 = "00000000-0000-4000-8000-0000000000d1"
	o1 = "00000000-0000-4000-8000-0000000000a1"
)

var secret = []byte("a secret")

func taxi(t *testing.T) *reasonr.Model {
	mach, err := reasonr.Build(testmodels.Dir("taxi"), reasonr.Options{Test: true})
	if err != nil {
		t.Fatal(err)
	}
	return mach
}

type env struct {
	t       *testing.T
	srv     *httptest.Server
	k       *kernel.Kernel
	metrics *telemetry.Prometheus
	x       kind.Executor
}

// wrapped is an executor whose errors the test sets.
type wrapped struct {
	kind.Executor
	commandErr, queryErr error
}

func (w *wrapped) Command(ctx context.Context, req *kind.CommandRequest) (*kind.CommandResult, error) {
	if w.commandErr != nil {
		return nil, w.commandErr
	}
	return w.Executor.Command(ctx, req)
}

func (w *wrapped) Query(ctx context.Context, req *kind.QueryRequest) (*reasonr.QueryOutcome, error) {
	if w.queryErr != nil {
		return nil, w.queryErr
	}
	return w.Executor.Query(ctx, req)
}

func newEnv(t *testing.T) (*env, *wrapped) {
	return newEnvWith(t, taxi(t), true)
}

// newEnvWith is an API server over the model m; allPublic is test mode:
// every public method is open, otherwise only those published in the contract.
func newEnvWith(t *testing.T, m *reasonr.Model, allPublic bool) (*env, *wrapped) {
	programs := kernel.NewPrograms()
	programs.Set(port.DefaultTenant, m)
	programs.Set("acme", m)
	store := memory.NewStore(programs)
	metrics := telemetry.NewPrometheus()
	k, err := kernel.New(kernel.Config{Programs: programs, Store: store, Timers: store, Hub: memory.NewHub(false),
		Clock: memory.NewClock(memory.DefaultNow), Metrics: metrics})
	if err != nil {
		t.Fatal(err)
	}
	x := &wrapped{Executor: k}
	chain := auth.Chain{
		"Test":   auth.Test{Catalog: programs},
		"Bearer": &auth.JWT{Keys: auth.StaticKey{K: secret}, Claims: auth.Claims{ActorType: "typ", Tenant: "tid", AnyTenant: "*"}, Catalog: programs},
	}
	s := httpapi.New(x, ports.EdgeDeps{Auth: chain, Metrics: metrics}, httpapi.Options{LongPoll: true, Metrics: metrics.Handler(), AllPublic: allPublic, MCP: true})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, k: k, metrics: metrics, x: x}, x
}

type response struct {
	status int
	header http.Header
	body   map[string]any
	raw    string
}

func (e *env) do(method, path, who, body string, header ...string) response {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if who != "" {
		req.Header.Set("Authorization", who)
	}
	for i := 0; i < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	r := response{status: resp.StatusCode, header: resp.Header, raw: string(raw)}
	_ = json.Unmarshal(raw, &r.body)
	return r
}

func (e *env) ok(r response) response {
	e.t.Helper()
	if r.status != http.StatusOK {
		e.t.Fatalf("response %d: %s", r.status, r.raw)
	}
	return r
}

func as(actor, id string) string { return "Test actor=" + actor + ":" + id }

const admin, matching = "Test role=admin", "Test role=matching"

const car = `{"plate":"A001AA77","model":"Kia Rio","carClass":"economy"}`

const ride = `{"orderId":"` + o1 + `","orderRoute":{"from":{"lat":55.75,"lon":37.62},"to":{"lat":55.75,"lon":37.62},"distanceKm":3},"requestedClass":"economy"}`

func errorName(r response) string {
	errs, _ := r.body["errors"].([]any)
	if len(errs) == 0 {
		return ""
	}
	e, _ := errs[0].(map[string]any)
	name, _ := e["name"].(string)
	return name
}

// TestTaxiFlow: an end-to-end order scenario through the API — commands,
// notifications to the initiator in the response, to other actors over
// WebSocket and long polling, queries with after.
func TestTaxiFlow(t *testing.T) {
	e, _ := newEnv(t)
	e.ok(e.do("POST", "/api/Customer/"+c1+"/SignUp", as("Customer", c1), `{"phoneNumber":"+79990000000"}`))
	e.ok(e.do("POST", "/api/Driver/"+d1+"/Register", admin, `{"driverName":"John","driverCar":`+car+`}`))
	e.ok(e.do("POST", "/api/Driver/"+d1+"/GoOnline", as("Driver", d1), ``))

	// The driver listens on a WebSocket, the customer uses long polling.
	ws := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/api/ws"
	read, closeWS, err := httpapi.DialWebSocket(context.Background(), ws, http.Header{"Authorization": {as("Driver", d1)}})
	if err != nil {
		t.Fatal(err)
	}
	defer closeWS()
	first := e.ok(e.do("GET", "/api/poll?wait=0s", as("Customer", c1), ""))
	cursor, _ := first.body["cursor"].(string)

	placed := e.ok(e.do("POST", "/api/Customer/"+c1+"/OrderTaxi", as("Customer", c1), ride))
	if placed.body["status"] != "accepted" || placed.body["position"] == "" {
		t.Fatalf("OrderTaxi: %s", placed.raw)
	}
	e.ok(e.do("POST", "/api/Trip/"+o1+"/DriverFound", matching, `{"driverId":"`+d1+`"}`))
	msg := make(chan []byte, 1)
	go func() {
		b, _ := read()
		msg <- b
	}()
	select {
	case b := <-msg:
		if !strings.Contains(string(b), `"name":"newOffer"`) || !strings.Contains(string(b), o1) {
			t.Fatalf("WebSocket: %s", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WebSocket: the notification did not come")
	}

	accepted := e.ok(e.do("POST", "/api/Driver/"+d1+"/Accept", as("Driver", d1), ""))
	polled := e.ok(e.do("GET", "/api/poll?wait=5s&cursor="+cursor, as("Customer", c1), ""))
	if !strings.Contains(polled.raw, `"name":"driverOnTheWay"`) || !strings.Contains(polled.raw, `"orderId":"`+o1+`"`) {
		t.Fatalf("long polling: %s", polled.raw)
	}
	card := e.ok(e.do("GET", "/api/Order/Card?orderId="+o1+"&after="+accepted.body["position"].(string), as("Customer", c1), ""))
	if card.body["status"] != "assigned" {
		t.Fatalf("Card: %s", card.raw)
	}
	page := e.ok(e.do("GET", "/api/Order/MyOrders?size=10", as("Customer", c1), ""))
	items, _ := page.body["items"].([]any)
	if len(items) != 1 || page.body["next"] != nil {
		t.Fatalf("MyOrders: %s", page.raw)
	}
}

// TestNotificationsInResponse: notifications to the initiator are in the response body.
func TestNotificationsInResponse(t *testing.T) {
	e, _ := newEnv(t)
	e.ok(e.do("POST", "/api/Customer/"+c1+"/SignUp", as("Customer", c1), `{"phoneNumber":"+79990000000"}`))
	r := e.ok(e.do("POST", "/api/Customer/"+c1+"/OrderTaxi", as("Customer", c1), ride))
	ns, _ := r.body["notifications"].([]any)
	if len(ns) != 1 || !strings.Contains(r.raw, `"name":"orderPlaced"`) || !strings.Contains(r.raw, `"price":{"amount":`) {
		t.Fatalf("notifications in the response: %s", r.raw)
	}
}

// TestStatuses checks the response codes of RFC §31.3.
func TestStatuses(t *testing.T) {
	e, x := newEnv(t)
	e.ok(e.do("POST", "/api/Customer/"+c1+"/SignUp", as("Customer", c1), `{"phoneNumber":"+79990000000"}`))
	e.ok(e.do("POST", "/api/Driver/"+d1+"/Register", admin, `{"driverName":"John","driverCar":`+car+`}`))
	for _, c := range []struct {
		name, method, path, who, body string
		status                        int
		error                         string
		contains                      string
	}{
		{"business error", "POST", "/api/Driver/00000000-0000-4000-8000-0000000000d2/Register", admin,
			`{"driverName":"Peter","driverCar":` + car + `}`, 422, "plateTaken", ""},
		{"forbidden", "POST", "/api/Driver/" + d1 + "/GoOnline", as("Customer", c1), ``, 403, "forbidden", ""},
		{"no argument", "POST", "/api/Customer/" + c1 + "/OrderTaxi", as("Customer", c1), `{}`, 400, "invalidArgument", `"rule":"required"`},
		{"wrong enumeration", "POST", "/api/Customer/" + c1 + "/OrderTaxi", as("Customer", c1),
			strings.Replace(ride, `"economy"`, `"rocket"`, 1), 400, "invalidArgument", `"path":"requestedClass","rule":"type"`},
		{"value object check", "POST", "/api/Customer/" + c1 + "/OrderTaxi", as("Customer", c1),
			strings.Replace(ride, `"lat":55.75`, `"lat":95`, 1), 400, "invalidArgument", `"rule":"check"`},
		{"nested path", "POST", "/api/Customer/" + c1 + "/OrderTaxi", as("Customer", c1),
			strings.Replace(ride, `"lon":37.62}`, `"lon":"x"}`, 1), 400, "invalidArgument", `"path":"orderRoute.from.lon"`},
		{"unknown argument", "POST", "/api/Customer/" + c1 + "/SignUp", as("Customer", c1), `{"phone":"1"}`, 400, "invalidArgument", `"rule":"unknown"`},
		{"wrong id", "POST", "/api/Customer/xyz/SignUp", as("Customer", c1), `{}`, 400, "invalidArgument", `"path":"id"`},
		{"wrong JSON", "POST", "/api/Customer/" + c1 + "/SignUp", as("Customer", c1), `{"phoneNumber":`, 400, "invalidArgument", ""},
		{"query error", "GET", "/api/Order/Card?orderId=" + o1, as("Customer", c1), ``, 422, "orderNotFound", ""},
		{"extra parameter", "GET", "/api/Order/Card?orderId=" + o1 + "&x=1", as("Customer", c1), ``, 400, "invalidArgument", `"path":"x","rule":"unknown"`},
		{"no credentials", "POST", "/api/Customer/" + c1 + "/SignUp", "", `{}`, 401, "", ""},
		{"wrong token", "POST", "/api/Customer/" + c1 + "/SignUp", "Bearer abc", `{}`, 401, "", ""},
		{"unknown tenant", "POST", "/api/Customer/" + c1 + "/SignUp", "Test tenant=nope role=admin", `{}`, 401, "", ""},
		{"no type", "POST", "/api/Nope/" + c1 + "/SignUp", admin, `{}`, 404, "", ""},
		{"no method", "GET", "/api/Order/Nope", admin, ``, 404, "", ""},
		{"private command", "POST", "/api/Order/" + o1 + "/place", admin, `{}`, 404, "", ""},
		{"command with GET", "GET", "/api/Driver/" + d1 + "/GoOnline", admin, ``, 405, "", ""},
	} {
		r := e.do(c.method, c.path, c.who, c.body)
		if r.status != c.status || errorName(r) != c.error || !strings.Contains(r.raw, c.contains) {
			t.Errorf("%s: %d %s", c.name, r.status, r.raw)
		}
	}
	// JWT: the actor from the claims.
	tok, _ := auth.Sign(map[string]any{"sub": c1, "typ": "Customer"}, secret)
	if r := e.do("GET", "/api/Customer/Profile", "Bearer "+tok, ""); r.status != 200 || r.body["phone"] != "+79990000000" {
		t.Errorf("JWT: %d %s", r.status, r.raw)
	}
	// An executor fault is 500 with an incident id, the details go to the log.
	x.commandErr = errors.New("a secret detail")
	r := e.do("POST", "/api/Driver/"+d1+"/GoOnline", as("Driver", d1), ``)
	if r.status != 500 || r.body["error"] != "internalError" || r.body["incident"] == "" || strings.Contains(r.raw, "secret") {
		t.Errorf("fault: %d %s", r.status, r.raw)
	}
	x.queryErr = port.ErrNotReady
	r = e.do("GET", "/api/Customer/Profile?after=5", as("Customer", c1), "")
	if r.status != 503 || r.header.Get("Retry-After") == "" || errorName(r) != "notReady" {
		t.Errorf("notReady: %d %s", r.status, r.raw)
	}
}

// TestIdempotency: a repeat with the same Idempotency-Key gets the same
// response without running the command.
func TestIdempotency(t *testing.T) {
	e, _ := newEnv(t)
	first := e.ok(e.do("POST", "/api/Customer/"+c1+"/SignUp", as("Customer", c1), `{"phoneNumber":"+79990000000"}`, "Idempotency-Key", "k1"))
	again := e.ok(e.do("POST", "/api/Customer/"+c1+"/SignUp", as("Customer", c1), `{"phoneNumber":"+79990000000"}`, "Idempotency-Key", "k1"))
	if again.body["position"] != first.body["position"] || again.header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("repeat: %s / %s", first.raw, again.raw)
	}
	other := e.ok(e.do("POST", "/api/Customer/"+c2+"/SignUp", as("Customer", c2), `{"phoneNumber":"+79990000001"}`, "Idempotency-Key", "k2"))
	if other.body["position"] == first.body["position"] {
		t.Fatal("another key got the previous response")
	}
}

// TestPollAndWSAccess: only actors get notifications.
func TestPollAndWSAccess(t *testing.T) {
	e, _ := newEnv(t)
	if r := e.do("GET", "/api/poll?wait=0s", admin, ""); r.status != 403 {
		t.Errorf("poll by a role: %d", r.status)
	}
	if r := e.do("GET", "/api/ws", as("Customer", c1), ""); r.status != http.StatusUpgradeRequired {
		t.Errorf("ws without Upgrade: %d", r.status)
	}
	ws := "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/api/ws?access_token=bad"
	if _, _, err := httpapi.DialWebSocket(context.Background(), ws, nil); err == nil {
		t.Error("ws with a wrong token: no error")
	}
	tok, _ := auth.Sign(map[string]any{"sub": c1, "typ": "Customer"}, secret)
	ws = "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/api/ws?access_token=" + tok
	_, closeWS, err := httpapi.DialWebSocket(context.Background(), ws, nil)
	if err != nil {
		t.Fatalf("ws with a token in the parameter: %v", err)
	}
	_ = closeWS()
	// A browser passes the token in the subprotocol (RFC §22).
	ws = "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/api/ws"
	_, closeWS, err = httpapi.DialWebSocket(context.Background(), ws, http.Header{"Sec-WebSocket-Protocol": {"reasonr, bearer." + tok}})
	if err != nil {
		t.Fatalf("ws with a token in the subprotocol: %v", err)
	}
	_ = closeWS()
	if _, _, err := httpapi.DialWebSocket(context.Background(), ws, http.Header{"Sec-WebSocket-Protocol": {"reasonr, bearer.bad"}}); err == nil {
		t.Error("ws with a wrong token in the subprotocol: no error")
	}
}

// TestWSCredentials: credentials from WebSocket subprotocols.
func TestWSCredentials(t *testing.T) {
	for in, want := range map[string]string{
		"reasonr, bearer.abc": "Bearer abc",
		"reasonr, test." + base64.RawURLEncoding.EncodeToString([]byte("actor=Customer:1")): "Test actor=Customer:1",
		"reasonr":          "",
		"reasonr, test.!!": "",
	} {
		h := http.Header{}
		h.Set("Sec-WebSocket-Protocol", in)
		if got := httpapi.WSCredentials(h); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// TestServiceEndpoints: OpenAPI, health, metrics.
func TestServiceEndpoints(t *testing.T) {
	e, _ := newEnv(t)
	doc := e.ok(e.do("GET", "/openapi.json", "", ""))
	paths, _ := doc.body["paths"].(map[string]any)
	if doc.body["openapi"] != "3.1.0" || paths["/api/Ordering.Customer/{id}/OrderTaxi"] == nil || paths["/api/Ordering.Order/Card"] == nil ||
		paths["/api/Ordering.Order/{id}/place"] != nil {
		t.Fatalf("OpenAPI: %d paths", len(paths))
	}
	e.ok(e.do("GET", "/healthz", "", ""))
	e.ok(e.do("GET", "/readyz", "", ""))
	e.ok(e.do("POST", "/api/Customer/"+c1+"/SignUp", as("Customer", c1), `{"phoneNumber":"+79990000000"}`))
	m := e.ok(e.do("GET", "/metrics", "", ""))
	for _, want := range []string{`reasonr_commands_total{outcome="accepted"} 1`, `reasonr_http_requests_total{route="command",code="200"} 1`} {
		if !strings.Contains(m.raw, want) {
			t.Errorf("metrics: no %s\n%s", want, m.raw)
		}
	}
}

// TestOpenAPIDump writes the OpenAPI of the examples and of their
// integration service contracts to the REASONR_OPENAPI_DIR directory, to
// check them with an external validator (npx @redocly/cli lint).
func TestOpenAPIDump(t *testing.T) {
	dir := os.Getenv("REASONR_OPENAPI_DIR")
	if dir == "" {
		t.Skip("REASONR_OPENAPI_DIR is not set")
	}
	for _, c := range []struct{ name, tenant string }{{"taxi", ""}, {"taxi", "acme"}, {"bank", ""}} {
		bc, err := reasonr.Build(testmodels.Dir(c.name), reasonr.Options{Extension: c.tenant, Test: true})
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.MarshalIndent(httpapi.OpenAPI(bc.Schema()), "", "  ")
		if err := os.WriteFile(filepath.Join(dir, c.name+c.tenant+".json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
		b, _ = json.MarshalIndent(httpapi.IntegrationsOpenAPI(bc.Schema()), "", "  ")
		if err := os.WriteFile(filepath.Join(dir, c.name+c.tenant+"-integrations.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
