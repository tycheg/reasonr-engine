package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tycheg/reasonr-engine/internal/testmodels"
)

// TestFakeTransport is the fake integration transport (RFC §18): in
// reasonrd --test the requests of an ordinary tenant are run by fakes set
// up through /_test/{tenant}/fakes, their answers come as commands with the
// system role, the payment fake's timer fires by the system clock; a
// request to a fake not created is only logged. Taxi tests in ephemeral
// tenants on the same server pass: their fakes are run by the cascade, not
// by the transport.
func TestFakeTransport(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "engine.yaml")
	if err := os.WriteFile(cfg, []byte(`
stores:
  main: { type: memory }
state: main
integrations:
  default: { type: fake }
ingress:
  - { type: http, listen: "127.0.0.1:0" }
telemetry:
  logs: { level: info }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	model := testmodels.Dir("taxi")
	base, stop := server(t, "--config", cfg, "--model", model, "--test")
	defer stop()
	const (
		c1 = "00000000-0000-4000-8000-0000000000c1"
		d1 = "00000000-0000-4000-8000-0000000000d1"
		o1 = "00000000-0000-4000-8000-0000000000a1"
		o2 = "00000000-0000-4000-8000-0000000000a2"
	)
	cust, drv, admin := "Test actor=Customer:"+c1, "Test actor=Driver:"+d1, "Test role=admin"
	must := func(code int, body string) string {
		t.Helper()
		if code != 200 {
			t.Fatalf("%d %s", code, body)
		}
		return body
	}
	waitStatus := func(order, status string, within time.Duration) {
		t.Helper()
		for deadline := time.Now().Add(within); ; time.Sleep(20 * time.Millisecond) {
			body := must(call(t, base, "GET", "/api/Order/Card?orderId="+order, cust, ``))
			if strings.Contains(body, `"status":"`+status+`"`) {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("order %s: want %s, %s", order, status, body)
			}
		}
	}
	ride := func(order string) string {
		return `{"orderId":"` + order + `","orderRoute":{"from":{"lat":55.75,"lon":37.62},"to":{"lat":55.75,"lon":37.62},"distanceKm":3},"requestedClass":"economy"}`
	}
	must(call(t, base, "POST", "/api/Driver/"+d1+"/Register", admin,
		`{"driverName":"John","driverCar":{"plate":"A001AA77","model":"Kia Rio","carClass":"economy"}}`))
	must(call(t, base, "POST", "/api/Driver/"+d1+"/GoOnline", drv, ``))
	must(call(t, base, "POST", "/api/Customer/"+c1+"/SignUp", cust, `{"phoneNumber":"+79990000000"}`))

	// The matching fake is not created: the request is recorded, there is no answer.
	must(call(t, base, "POST", "/api/Customer/"+c1+"/OrderTaxi", cust, ride(o1)))
	time.Sleep(300 * time.Millisecond)
	waitStatus(o1, "searching", 0)
	must(call(t, base, "POST", "/api/Order/"+o1+"/Cancel", cust, `{"reason":"changed my mind"}`))

	if code, body := call(t, base, "POST", "/_test/default/fakes/Matching/Setup", "", `{"freeDrivers":["`+d1+`"]}`); code != 200 || body != "{\"errors\":[]}\n" {
		t.Fatalf("Matching.Setup: %d %s", code, body)
	}
	must(call(t, base, "POST", "/_test/default/fakes/Payments/Setup", "", `{"limitAmount":"1000"}`))
	if code, body := call(t, base, "POST", "/_test/nobody/fakes/Payments/Setup", "", `{"limitAmount":"1"}`); code != 404 {
		t.Fatalf("unknown tenant: %d %s", code, body)
	}
	must(call(t, base, "POST", "/api/Customer/"+c1+"/OrderTaxi", cust, ride(o2)))
	waitStatus(o2, "offered", 5*time.Second) // the matching fake answered DriverFound
	must(call(t, base, "POST", "/api/Driver/"+d1+"/Accept", drv, ``))
	must(call(t, base, "POST", "/api/Driver/"+d1+"/FinishRide", drv, ``))
	waitStatus(o2, "completed", 5*time.Second)
	waitStatus(o2, "paid", 10*time.Second) // after 5 s the payment fake's timer called PaymentCaptured

	runHTTP(t, base, model, "")
}
