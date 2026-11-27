package httpapi_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	reasonr "github.com/tycheg/reasonr-compiler"

	"github.com/tycheg/reasonr-engine/internal/testmodels"
)

// TestContract checks the external API contract (RFC §31.3.1): outside test
// mode only what is published is open; version 2026-11-12 was released
// before renaming the parameter Cancel(reason) → Cancel(cancelReason was
// reason), and its client passes the old name; a deprecated method comes
// with Deprecation and Sunset.
func TestContract(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "taxi")
	if err := os.CopyFS(dir, os.DirFS(testmodels.Dir("taxi"))); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REASONR_MODULES", testmodels.Modules())
	write := func(rel, text string) {
		p := filepath.Join(dir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("api/contract.json", `{"version":1,"publish":["Ordering.Customer.*","Ordering.Order.Cancel","Ordering.Order.Card"],
		"deprecate":[{"method":"Ordering.Order.Card","sunset":"2099-01-01"}]}`)
	// The version is a snapshot before the rename.
	m, err := reasonr.Build(dir, reasonr.Options{Test: true})
	if err != nil {
		t.Fatalf("model: %v", err)
	}
	s := m.Schema()
	v, _ := json.Marshal(reasonr.APISnapshot(s, s.API, "2026-11-12"))
	write("api/versions/2026-11-12.json", string(v))
	order := filepath.Join(dir, "contexts", "Ordering", "order.rr")
	src, _ := os.ReadFile(order)
	text := strings.Replace(string(src), "Cancel(reason string)", "Cancel(cancelReason string was reason)", 1)
	text = strings.Replace(text, "orderCancelled(reason: reason, at: now)", "orderCancelled(reason: cancelReason, at: now)", 1)
	write(filepath.Join("contexts", "Ordering", "order.rr"), text)
	for _, f := range []string{"admin.test.rr", "ride.test.rr", "ui.test.rr", "moscow.demo.rr"} {
		p := filepath.Join("contexts", "Ordering", f)
		b, _ := os.ReadFile(filepath.Join(dir, p))
		write(p, strings.ReplaceAll(string(b), ".Cancel(reason:", ".Cancel(cancelReason:"))
	}
	mach, err := reasonr.Build(dir, reasonr.Options{Test: true})
	if err != nil {
		t.Fatal(err)
	}
	e, _ := newEnvWith(t, mach, false)

	e.ok(e.do("POST", "/api/Customer/"+c1+"/SignUp", as("Customer", c1), `{"phoneNumber":"+79990000000"}`))
	e.ok(e.do("POST", "/api/Customer/"+c1+"/OrderTaxi", as("Customer", c1), ride))
	if r := e.do("GET", "/api/Order/List?wanted=searching", admin, ""); r.status != http.StatusNotFound {
		t.Fatalf("unpublished Order.List: %d %s", r.status, r.raw)
	}
	card := e.ok(e.do("GET", "/api/Order/Card?orderId="+o1, as("Customer", c1), ""))
	if card.header.Get("Deprecation") != "true" || !strings.Contains(card.header.Get("Sunset"), "2099") {
		t.Fatalf("deprecated Card: %v", card.header)
	}
	e.ok(e.do("POST", "/api/Order/"+o1+"/Cancel", as("Customer", c1), `{"reason":"changed my mind"}`, "Reasonr-Version", "2026-11-12"))
	if got := e.ok(e.do("GET", "/api/Order/Card?orderId="+o1, as("Customer", c1), "")); got.body["status"] != "cancelled" {
		t.Fatalf("Cancel of version 2026-11-12: %s", got.raw)
	}
	if r := e.do("GET", "/api/Order/Card?orderId="+o1, as("Customer", c1), "", "Reasonr-Version", "1999-01-01"); r.status != http.StatusBadRequest {
		t.Fatalf("unknown version: %d %s", r.status, r.raw)
	}
	if m := e.do("GET", "/metrics", "", ""); !strings.Contains(m.raw, `reasonr_api_calls_total{method="Ordering.Order.Cancel",version="2026-11-12"} 1`) {
		t.Fatalf("the API call metric:\n%s", m.raw)
	}
	doc := e.ok(e.do("GET", "/openapi.json", "", "", "Reasonr-Version", "2026-11-12"))
	paths, _ := doc.body["paths"].(map[string]any)
	if paths["/api/Ordering.Order/List"] != nil || paths["/api/Ordering.Order/{id}/Cancel"] == nil || !strings.Contains(doc.raw, `"reason"`) {
		t.Fatalf("version OpenAPI: %v", paths)
	}
}
