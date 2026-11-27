package appapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/auth"
	"github.com/tycheg/reasonr-engine/internal/adapters/http/appapi"
	"github.com/tycheg/reasonr-engine/internal/adapters/http/httpapi"
	"github.com/tycheg/reasonr-engine/internal/adapters/memory"
	"github.com/tycheg/reasonr-engine/internal/features"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

const flagsModel = `role admin

## Tips
feature tips "2026-11-07"

value Card
  tip int

aggregate Order
  id uuid
  tip int

  command(admin) LeaveTip(amount int)
    if tips
      tipped(amount: amount)

  event tipped(amount int)
    tip = amount

  query(admin) Show(orderId Order.Id) Card
    return Card(tip: [orderId]?.tip ?? 0)

screen NewOrder "Order"
  Form [new Order.Id].LeaveTip "Tip" if tips

screen Tips "Tips"
  Form [new Order.Id].LeaveTip

app(admin) Admin "Admin"
  Menu
    NewOrder
    Tips if tips

test "Tips"
  feature tips
  let o1 = new Order.Id
  when
    [o1].LeaveTip(amount: 1) as admin
`

// TestFeatures: if conditions with a feature flag in a screen and in the
// app menu are evaluated by the caller's flags; the manifest lists the
// switched-on flags (RFC §23.4).
func TestFeatures(t *testing.T) {
	m, err := reasonr.LoadFS(fstest.MapFS{"m.rr": {Data: []byte(flagsModel)}}, "m")
	if err != nil {
		t.Fatal(err)
	}
	mach, err := m.Build(reasonr.Options{Test: true})
	if err != nil {
		t.Fatal(err)
	}
	programs := kernel.NewPrograms()
	programs.Set(port.DefaultTenant, mach)
	store := memory.NewStore(programs)
	flags := features.New(store, nil, nil)
	k, err := kernel.New(kernel.Config{Programs: programs, Store: store, Timers: store, Hub: memory.NewHub(false),
		Clock: memory.NewClock(memory.DefaultNow), Features: flags})
	if err != nil {
		t.Fatal(err)
	}
	cat := catalog{mach.Schema(): mach}
	app := func(x kind.Executor, deps ports.EdgeDeps) http.Handler {
		return appapi.New(x, deps, appapi.Options{Catalog: cat}).Handler()
	}
	s := httpapi.New(k, ports.EdgeDeps{Auth: auth.Chain{"Test": auth.Test{Catalog: programs}}},
		httpapi.Options{AllPublic: true, Handlers: map[string]func(kind.Executor, ports.EdgeDeps) http.Handler{"/app/": app}})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	get := func(path string) map[string]any {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("Authorization", "Test role=admin")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != 200 {
			t.Fatalf("GET %s: %d %v", path, resp.StatusCode, err)
		}
		return out
	}
	hidden := func(states any) int {
		n := 0
		for _, st := range asMap(states) {
			if asMap(st)["hidden"] == true {
				n++
			}
		}
		return n
	}
	man := get("/app/Admin")
	if man["features"] != nil || hidden(man["states"]) != 1 {
		t.Fatalf("flag off: manifest %v %v", man["features"], man["states"])
	}
	data := get("/app/Admin/NewOrder")
	if hidden(data["nodes"]) != 1 {
		t.Fatalf("flag off: nodes %v", data["nodes"])
	}
	if err := flags.Change(context.Background(), port.FeatureChange{Feature: "tips", On: true, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	man = get("/app/Admin")
	if f, _ := man["features"].([]any); len(f) != 1 || f[0] != "tips" || hidden(man["states"]) != 0 {
		t.Fatalf("flag on: manifest %v %v", man["features"], man["states"])
	}
	if data := get("/app/Admin/NewOrder"); hidden(data["nodes"]) != 0 {
		t.Fatalf("flag on: nodes %v", data["nodes"])
	}
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}
