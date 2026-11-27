package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tycheg/reasonr-engine/internal/adapters/auth"
	"github.com/tycheg/reasonr-engine/internal/adapters/http/httpapi"
	"github.com/tycheg/reasonr-engine/internal/adapters/memory"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/port"
)

// TestTimeOffset: the Reasonr-Time-Offset header moves the clock of the
// unit of work only for tenants that allow it (test mode, demo: true — RFC
// §27.5); a command's timer is stored by the real moment with the scenario
// offset. Other tenants and a wrong value get 400.
func TestTimeOffset(t *testing.T) {
	m := taxi(t)
	programs := kernel.NewPrograms()
	programs.Set(port.DefaultTenant, m)
	programs.Set("demo", m)
	store := memory.NewStore(programs)
	k, err := kernel.New(kernel.Config{Programs: programs, Store: store, Timers: store, Hub: memory.NewHub(false),
		Clock: memory.NewClock(memory.DefaultNow)})
	if err != nil {
		t.Fatal(err)
	}
	s := httpapi.New(k, ports.EdgeDeps{Auth: auth.Chain{"Test": auth.Test{Catalog: programs}}},
		httpapi.Options{AllPublic: true, TimeOffset: func(t port.Tenant) bool { return t == "demo" }})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	e := &env{t: t, srv: srv, k: k}
	signUp := func(tenant, offset string) response {
		return e.do("POST", "/api/Customer/"+c1+"/SignUp", "Test tenant="+tenant+" actor=Customer:"+c1, `{"phoneNumber":"+79990000000"}`,
			httpapi.TimeOffsetHeader, offset)
	}
	if r := signUp(string(port.DefaultTenant), "-30d"); r.status != http.StatusBadRequest {
		t.Fatalf("a tenant without demo: %d %s", r.status, r.raw)
	}
	if r := signUp("demo", "-30 days"); r.status != http.StatusBadRequest {
		t.Fatalf("a wrong offset: %d %s", r.status, r.raw)
	}
	e.ok(signUp("demo", "-30d"))
	e.ok(e.do("POST", "/api/Customer/"+c1+"/OrderTaxi", "Test tenant=demo actor=Customer:"+c1, ride, httpapi.TimeOffsetHeader, "-30d"))
	// The driver search timer — in 60 s by the scenario clock, which is 30 days behind.
	scenario := memory.DefaultNow.Add(-30 * 24 * time.Hour)
	ts, err := store.Lease(context.Background(), port.LeaseRequest{Tenant: "demo", Now: scenario.Add(61 * time.Second), Limit: 10, TTL: time.Second, Scenario: true})
	if err != nil || len(ts) != 1 || ts[0].Offset != -30*24*time.Hour || !ts[0].At.Equal(scenario.Add(60*time.Second)) {
		t.Fatalf("scenario timer: %v %+v", err, ts)
	}
}
