package appapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/auth"
	"github.com/tycheg/reasonr-engine/internal/adapters/http/appapi"
	"github.com/tycheg/reasonr-engine/internal/adapters/http/httpapi"
	"github.com/tycheg/reasonr-engine/internal/adapters/memory"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/internal/testmodels"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

type catalog map[*reasonr.Schema]*reasonr.Model

func (c catalog) UI(sc *reasonr.Schema) *reasonr.Model { return c[sc] }

// counting is an executor that counts queries.
type counting struct {
	kind.Executor
	queries atomic.Int32
}

func (c *counting) Query(ctx context.Context, r *kind.QueryRequest) (*reasonr.QueryOutcome, error) {
	c.queries.Add(1)
	return c.Executor.Query(ctx, r)
}

type env struct {
	t   *testing.T
	srv *httptest.Server
	ui  *reasonr.UI
	x   *counting
}

func newEnv(t *testing.T) *env {
	t.Helper()
	mach, err := reasonr.Build(testmodels.Dir("taxi"), reasonr.Options{Test: true})
	if err != nil {
		t.Fatal(err)
	}
	programs := kernel.NewPrograms()
	programs.Set(port.DefaultTenant, mach)
	store := memory.NewStore(programs)
	k, err := kernel.New(kernel.Config{Programs: programs, Store: store, Timers: store, Hub: memory.NewHub(false),
		Clock: memory.NewClock(memory.DefaultNow)})
	if err != nil {
		t.Fatal(err)
	}
	x := &counting{Executor: k}
	cat := catalog{mach.Schema(): mach}
	app := func(x kind.Executor, deps ports.EdgeDeps) http.Handler {
		return appapi.New(x, deps, appapi.Options{Catalog: cat}).Handler()
	}
	s := httpapi.New(x, ports.EdgeDeps{Auth: auth.Chain{"Test": auth.Test{Catalog: programs}}},
		httpapi.Options{AllPublic: true, Handlers: map[string]func(kind.Executor, ports.EdgeDeps) http.Handler{"/app/": app, "/ui/": app}})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return &env{t: t, srv: srv, ui: mach.UI(), x: x}
}

type response struct {
	status int
	body   map[string]any
	raw    string
}

func (e *env) do(method, path, who, body string) response {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if who != "" {
		req.Header.Set("Authorization", who)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	r := response{status: resp.StatusCode, raw: string(raw)}
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

// nodes are the node states in screen data.
func nodes(r response) map[string]any {
	n, _ := r.body["nodes"].(map[string]any)
	return n
}

const (
	c1 = "00000000-0000-0000-0000-0000000000c1"
	o1 = "00000000-0000-0000-0000-0000000000a1"
	s1 = "00000000-0000-0000-0000-0000000000e1"
)

var (
	customer = "Test actor=Customer:" + c1
	adminS   = "Test actor=Staff:" + s1 + ", role=admin"
	support  = "Test actor=Staff:" + s1 + ", role=support"
	staff    = "Test actor=Staff:" + s1
)

const ride = `{"from":{"lat":55.75,"lon":37.62},"to":{"lat":55.8,"lon":37.6},"distanceKm":10}`

// placeID is the node number of an app place on a screen.
func (e *env) placeID(app, place string) string {
	for _, p := range e.ui.App(app).Places {
		if p.Name == place {
			return itoa(p.Node)
		}
	}
	e.t.Fatalf("no place %s", place)
	return ""
}

func itoa(n int) string { return strconv.Itoa(n) }

func (e *env) order() {
	e.t.Helper()
	e.ok(e.do("POST", "/api/Customer/"+c1+"/SignUp", customer, `{"phoneNumber":"+79990000001"}`))
	e.ok(e.do("POST", "/api/Customer/"+c1+"/OrderTaxi", customer, `{"orderId":"`+o1+`","orderRoute":`+ride+`,"requestedClass":"economy"}`))
}

// TestManifestAndScreens: the manifest and screen descriptions by hash;
// only the app's actors and roles are admitted to it.
func TestManifestAndScreens(t *testing.T) {
	e := newEnv(t)
	if r := e.do("GET", "/app/Admin", "", ""); r.status != http.StatusUnauthorized {
		t.Errorf("without sign-in: %d", r.status)
	}
	if r := e.do("GET", "/app/Admin", customer, ""); r.status != http.StatusForbidden {
		t.Errorf("a customer in the admin app: %d", r.status)
	}
	if r := e.do("GET", "/app/Nope", adminS, ""); r.status != http.StatusNotFound {
		t.Errorf("no app: %d", r.status)
	}
	m := e.ok(e.do("GET", "/app/Admin", adminS, ""))
	screens, _ := m.body["screens"].(map[string]any)
	hash, _ := screens["Ordering.AdminOrder"].(string)
	if hash == "" || m.body["title"] != "Admin" {
		t.Fatalf("manifest: %s", m.raw)
	}
	d := e.ok(e.do("GET", "/app/Admin/screens/AdminOrder."+hash+".json", adminS, ""))
	if d.body["title"] != "Order" {
		t.Errorf("description: %s", d.raw)
	}
	if r := e.do("GET", "/app/Admin/screens/AdminOrder.0000.json", adminS, ""); r.status != http.StatusNotFound {
		t.Errorf("an old hash: %d", r.status)
	}
	if r := e.do("GET", "/app/Admin/screens/Shift."+hash+".json", adminS, ""); r.status != http.StatusNotFound {
		t.Errorf("a screen of another app: %d", r.status)
	}
	if en := e.ok(e.do("GET", "/app/Admin?lang=de", adminS, "")); en.body["lang"] != "de" {
		t.Errorf("language: %s", en.raw)
	}
	if r := e.ok(e.do("GET", "/app/_login", "", "")); r.raw != "{}" {
		t.Errorf("_login: %s", r.raw)
	}
	if r := e.ok(e.do("GET", "/app/Admin/openapi.json", adminS, "")); !strings.Contains(r.raw, "Admin.AdminOrder:Order.Edit") {
		t.Errorf("the app OpenAPI: %.200s", r.raw)
	}
}

// TestScreenData: screen data in one request, by role; a node whose method
// is not accessible is hidden and its nested queries do not run; the
// server evaluates if.
func TestScreenData(t *testing.T) {
	e := newEnv(t)
	e.order()
	card := e.placeID("Admin", "Admin.AdminOrder:Order.Details")
	edit := e.placeID("Admin", "Admin.AdminOrder:Order.Edit")
	for _, who := range []string{adminS, support} {
		r := e.ok(e.do("GET", "/app/Admin/AdminOrder?orderId="+o1, who, ""))
		st, _ := nodes(r)[card].(map[string]any)
		data, _ := st["data"].(map[string]any)
		if data["status"] != "searching" || data["requestedClass"] != "economy" {
			t.Fatalf("%s: card data: %s", who, r.raw)
		}
		if es, _ := nodes(r)[edit].(map[string]any); es != nil && es["hidden"] == true {
			t.Errorf("%s: Edit is hidden with status == searching: %s", who, r.raw)
		}
	}
	before := e.x.queries.Load()
	r := e.ok(e.do("GET", "/app/Admin/AdminOrder?orderId="+o1, staff, ""))
	if st, _ := nodes(r)[card].(map[string]any); st["hidden"] != true {
		t.Errorf("staff without a role see the card: %s", r.raw)
	}
	if n := e.x.queries.Load() - before; n != 0 {
		t.Errorf("a hidden node ran %d queries", n)
	}
	if r := e.do("GET", "/app/Admin/AdminOrder", adminS, ""); r.status != http.StatusBadRequest {
		t.Errorf("without a screen parameter: %d %s", r.status, r.raw)
	}
	// Table rows: the visibility of Confirm by the status of each row.
	list := e.ok(e.do("GET", "/app/Admin/AdminOrders", adminS, ""))
	table := e.placeID("Admin", "Admin.AdminOrders:Order.List")
	cancel := e.placeID("Admin", "Admin.AdminOrders:Order.Cancel")
	st, _ := nodes(list)[table].(map[string]any)
	rows, _ := st["rows"].(map[string]any)
	vis, _ := rows[cancel].([]any)
	if len(vis) != 1 || vis[0] != true {
		t.Errorf("rows: %s", list.raw)
	}
	// After cancelling, the if condition hides Edit on the card and Confirm in the row.
	e.ok(e.do("POST", "/api/Order/"+o1+"/Cancel", adminS, `{"reason":"test"}`))
	r = e.ok(e.do("GET", "/app/Admin/AdminOrder?orderId="+o1, adminS, ""))
	if es, _ := nodes(r)[edit].(map[string]any); es == nil || es["hidden"] != true {
		t.Errorf("Edit is visible for a cancelled order: %s", r.raw)
	}
	q := url.QueryEscape(`{"wanted":"cancelled","pageReq":{"size":20,"cursor":null}}`)
	page := e.ok(e.do("GET", "/app/Admin/AdminOrders/"+table+"?args="+q, adminS, ""))
	rows, _ = page.body["rows"].(map[string]any)
	if vis, _ := rows[cancel].([]any); len(vis) != 1 || vis[0] != false {
		t.Errorf("Confirm in the row of a cancelled order: %s", page.raw)
	}
}

// TestPlaces: a place's command and query; a method that is not among the
// app's places cannot be called through the app API.
func TestPlaces(t *testing.T) {
	e := newEnv(t)
	e.order()
	edit := e.placeID("Admin", "Admin.AdminOrder:Order.Edit")
	r := e.ok(e.do("POST", "/app/Admin/AdminOrder/"+edit, adminS, `{"target":"`+o1+`","args":{"orderRoute":`+ride+`,"requestedClass":"comfort"}}`))
	if r.body["status"] != "accepted" {
		t.Fatalf("Edit: %s", r.raw)
	}
	after, _ := r.body["position"].(string)
	data := e.ok(e.do("GET", "/app/Admin/AdminOrder?orderId="+o1+"&after="+after, adminS, ""))
	card := e.placeID("Admin", "Admin.AdminOrder:Order.Details")
	if st, _ := nodes(data)[card].(map[string]any); st["data"].(map[string]any)["requestedClass"] != "comfort" {
		t.Errorf("after Edit: %s", data.raw)
	}
	// A node key instead of the number.
	if r := e.do("POST", "/app/Admin/AdminOrder/Order.Edit", adminS, `{"target":"`+o1+`","args":{"orderRoute":`+ride+`,"requestedClass":"comfort"}}`); r.status != http.StatusOK {
		t.Errorf("by key: %d %s", r.status, r.raw)
	}
	// A business error — as in the model API.
	if r := e.do("POST", "/app/Admin/AdminOrder/"+edit, adminS, `{"target":"`+o1+`","args":{"orderRoute":`+ride+`}}`); r.status != http.StatusBadRequest {
		t.Errorf("without an argument: %d %s", r.status, r.raw)
	}
	// Not a place: the card is a query, a POST to it is 404; no node; another app's screen.
	if r := e.do("POST", "/app/Admin/AdminOrder/"+e.placeID("Admin", "Admin.AdminOrder:Order.Details"), adminS, `{}`); r.status != http.StatusNotFound {
		t.Errorf("POST to a query: %d", r.status)
	}
	if r := e.do("POST", "/app/Admin/AdminOrder/999", adminS, `{}`); r.status != http.StatusNotFound {
		t.Errorf("no node: %d", r.status)
	}
	if r := e.do("POST", "/app/Admin/Shift/1", adminS, `{}`); r.status != http.StatusNotFound {
		t.Errorf("a screen of another app: %d", r.status)
	}
	// A place query: the next page with a filter.
	list := e.placeID("Admin", "Admin.AdminOrders:Order.List")
	q := url.QueryEscape(`{"wanted":"searching","pageReq":{"size":1,"cursor":null}}`)
	page := e.ok(e.do("GET", "/app/Admin/AdminOrders/"+list+"?args="+q, adminS, ""))
	items, _ := page.body["data"].(map[string]any)["items"].([]any)
	if len(items) != 1 {
		t.Errorf("page: %s", page.raw)
	}
	// A primitive's query parameter: search of the EntityPicker of field customer.
	picker := ""
	for _, n := range e.ui.Screen("AdminNewOrder").Nodes[0].Children {
		if n.Field == "customer" {
			picker = itoa(n.ID)
		}
	}
	found := e.ok(e.do("GET", "/app/Admin/AdminNewOrder/"+picker+"?param=search&args="+url.QueryEscape(`{"phoneNumber":"+79990000001"}`), adminS, ""))
	if rows, _ := found.body["data"].([]any); len(rows) != 1 {
		t.Errorf("customer search: %s", found.raw)
	}
}
