package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/tycheg/reasonr-engine/internal/adapters/auth"
)

// TestSystemToken: the integration service with a token for every tenant
// answers in the tenant from X-Reasonr-Tenant; a token for every tenant
// that is not an external system's is 403.
func TestSystemToken(t *testing.T) {
	e, _ := newEnv(t)
	in := func(tenant string) string { return "Test tenant=" + tenant + " actor=Customer:" + c1 }
	e.ok(e.do("POST", "/api/Customer/"+c1+"/SignUp", in("acme"), `{"phoneNumber":"+79990000000"}`))
	e.ok(e.do("POST", "/api/Customer/"+c1+"/OrderTaxi", in("acme"), ride))
	system, _ := auth.Sign(map[string]any{"tid": "*", "roles": []any{"matching"}}, secret)
	if r := e.do("POST", "/api/Trip/"+o1+"/NoDriverFound", "Bearer "+system, "", "X-Reasonr-Tenant", "acme"); r.status != http.StatusOK {
		t.Fatalf("a system answer in acme: %d %s", r.status, r.raw)
	}
	// The answer is applied to the order of tenant acme: the search is over.
	if card := e.ok(e.do("GET", "/api/Order/Card?orderId="+o1, in("acme"), "")); card.body["status"] == "searching" {
		t.Fatalf("the order in acme: %s", card.raw)
	}
	if r := e.do("POST", "/api/Trip/"+o1+"/NoDriverFound", "Bearer "+system, ""); r.status != http.StatusUnauthorized {
		t.Fatalf("without X-Reasonr-Tenant: %d %s", r.status, r.raw)
	}
	admin, _ := auth.Sign(map[string]any{"tid": "*", "roles": []any{"admin"}}, secret)
	if r := e.do("POST", "/api/Trip/"+o1+"/NoDriverFound", "Bearer "+admin, "", "X-Reasonr-Tenant", "acme"); r.status != http.StatusForbidden {
		t.Fatalf("a token for every tenant with role admin: %d %s", r.status, r.raw)
	}
	doc := e.ok(e.do("GET", "/openapi/integrations.json", "", ""))
	paths, _ := doc.body["paths"].(map[string]any)
	if len(paths) != 2 || paths["/requests/Matching/findDriver"] == nil || paths["/requests/Payments/charge"] == nil {
		t.Fatalf("the integration service contract: %v", paths)
	}
}
