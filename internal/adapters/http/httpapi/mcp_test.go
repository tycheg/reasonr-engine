package httpapi_test

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMCP checks the model's MCP server (RFC §31.3.2): a taxi customer sees
// their tools (not the driver's or the admin's), orders a taxi and reads
// the order card; a repeated call in the same session does not run the
// command twice.
func TestMCP(t *testing.T) {
	e, _ := newEnv(t)
	cust := as("Customer", c1)
	rpc := func(who, session, id, method, params string) map[string]any {
		t.Helper()
		body := `{"jsonrpc":"2.0","id":` + id + `,"method":"` + method + `","params":` + params + `}`
		r := e.do("POST", "/mcp", who, body, "Mcp-Session-Id", session)
		if r.status != 200 || r.body["error"] != nil {
			t.Fatalf("%s: %d %s", method, r.status, r.raw)
		}
		return r.body["result"].(map[string]any)
	}
	init := e.do("POST", "/mcp", cust, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	session := init.header.Get("Mcp-Session-Id")
	if init.status != 200 || session == "" || !strings.Contains(init.raw, `"tools"`) {
		t.Fatalf("initialize: %d %v %s", init.status, init.header, init.raw)
	}
	list := rpc(cust, session, "2", "tools/list", `{}`)
	b, _ := json.Marshal(list)
	names := string(b)
	for _, want := range []string{`"Ordering.Customer.OrderTaxi"`, `"Ordering.Order.Card"`, `"readOnlyHint":true`} {
		if !strings.Contains(names, want) {
			t.Fatalf("the customer's tools/list has no %s", want)
		}
	}
	for _, not := range []string{`"Dispatch.Driver.Register"`, `"Ordering.Order.List"`} {
		if strings.Contains(names, not) {
			t.Fatalf("the customer's tools/list has someone else's %s", not)
		}
	}
	call := func(id, name, args string) map[string]any {
		return rpc(cust, session, id, "tools/call", `{"name":"`+name+`","arguments":`+args+`}`)
	}
	if r := call("3", "Ordering.Customer.SignUp", `{"id":"`+c1+`","phoneNumber":"+79990000000"}`); r["isError"] != false {
		t.Fatalf("SignUp: %v", r)
	}
	order := `{"id":"` + c1 + `","orderId":"` + o1 + `","orderRoute":{"from":{"lat":55.75,"lon":37.62},"to":{"lat":55.8,"lon":37.6},"distanceKm":3},"requestedClass":"economy"}`
	first := call("4", "Ordering.Customer.OrderTaxi", order)
	again := call("4", "Ordering.Customer.OrderTaxi", order) // the agent repeats the call
	if first["isError"] != false || again["isError"] != false {
		t.Fatalf("OrderTaxi: %v / %v", first, again)
	}
	card := call("5", "Ordering.Order.Card", `{"orderId":"`+o1+`"}`)
	res, _ := card["structuredContent"].(map[string]any)["result"].(map[string]any)
	if card["isError"] != false || res["status"] != "searching" {
		t.Fatalf("Card: %v", card)
	}
	// A second command in another session is a model error: the customer already has an order.
	if r := rpc(cust, "other", "4", "tools/call", `{"name":"Ordering.Customer.OrderTaxi","arguments":`+order+`}`); r["isError"] != true {
		t.Fatalf("a second order: %v", r)
	}
}
