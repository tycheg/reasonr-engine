package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tycheg/reasonr-engine/internal/host"
	"github.com/tycheg/reasonr-engine/internal/testmodels"
	"github.com/tycheg/reasonr-engine/internal/testrun"
)

// TestDev: reasonrd dev — the model in memory in test mode, tenant acme,
// Matching on an "integration service" (--integrations), other systems on
// fakes; /ui/ from a dev server (--ui url). A model change rebuilds the
// engine, an error in the model keeps the previous one.
func TestDev(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "taxi")
	copyModel(t, testmodels.Dir("taxi"), dir)
	t.Setenv("REASONR_MODULES", testmodels.Modules())
	var mu sync.Mutex
	var requests []string
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.Path+" "+strings.Fields(r.Header.Get("Authorization") + " -")[0])
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer svc.Close()
	vite := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "vite "+r.URL.Path)
	}))
	defer vite.Close()

	addr, err := testrun.FreeAddr()
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + addr
	ctx, cancel := context.WithCancel(context.Background())
	var log syncBuffer
	starts := make(chan struct{}, 10)
	done := make(chan int, 1)
	go func() {
		done <- runWith(ctx, []string{"dev", "--listen", addr, "--watch", "20ms",
			"--integrations", "Matching=" + svc.URL, "--ui", vite.URL, dir}, os.Stdout, &log,
			func(*host.Engine) { starts <- struct{}{} })
	}()
	defer func() {
		cancel()
		select {
		case code := <-done:
			if code != 0 {
				t.Errorf("exit code %d", code)
			}
		case <-time.After(15 * time.Second):
			t.Error("reasonrd dev did not stop")
		}
		if t.Failed() {
			t.Log(log.String())
		}
	}()
	started := func() {
		t.Helper()
		select {
		case <-starts:
		case <-time.After(15 * time.Second):
			t.Fatal("the engine did not start")
		}
	}
	started()

	c1, o1 := "00000000-0000-0000-0000-0000000000c1", "00000000-0000-0000-0000-0000000000a1"
	customer := "Test actor=Customer:" + c1
	if code, body := call(t, base, "POST", "/api/Customer/"+c1+"/SignUp", customer, `{"phoneNumber":"+79990000000"}`); code != 200 {
		t.Fatalf("SignUp: %d %s", code, body)
	}
	if code, body := call(t, base, "POST", "/api/Customer/"+c1+"/SignUp", customer+", tenant=acme", `{"phoneNumber":"+79990000000"}`); code != 200 {
		t.Fatalf("SignUp in acme: %d %s", code, body)
	}
	if code, body := call(t, base, "POST", "/api/Customer/"+c1+"/OrderTaxi", customer,
		`{"orderId":"`+o1+`","orderRoute":{"from":{"lat":55.75,"lon":37.62},"to":{"lat":55.8,"lon":37.6},"distanceKm":3},"requestedClass":"economy"}`); code != 200 {
		t.Fatalf("OrderTaxi: %d %s", code, body)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		mu.Lock()
		got := strings.Join(requests, "; ")
		mu.Unlock()
		if got == "/requests/Matching/findDriver Bearer" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the integration service got: %q", got)
		}
	}
	if code, body := call(t, base, "GET", "/ui/Taxi", customer, ""); code != 200 || body != "vite /ui/Taxi" {
		t.Fatalf("/ui/: %d %q", code, body)
	}

	// A model change means a new engine; the in-memory data is reset.
	types := filepath.Join(dir, "contexts", "Ordering", "types.rr")
	src, err := os.ReadFile(types)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(types, append(src, "\nvalue DevProbe\n  probe int\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	started()
	if code, body := call(t, base, "POST", "/api/Customer/"+c1+"/SignUp", customer, `{"phoneNumber":"+79990000000"}`); code != 200 {
		t.Fatalf("SignUp after the rebuild: %d %s", code, body)
	}

	// An error in the model: the previous engine keeps working.
	if err := os.WriteFile(types, append(src, "\nvalue Broken\n  x Nope\n"...), 0o644); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(log.String(), "the previous program keeps running"); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("no message about the model error")
		}
	}
	if code, body := call(t, base, "GET", "/api/Customer/Profile", customer, ""); code != 200 {
		t.Fatalf("the previous engine: %d %s", code, body)
	}
	select {
	case <-starts:
		t.Fatal("the engine was rebuilt on a model with an error")
	default:
	}
}

// copyModel copies the model module (the *.rr files with subdirectories).
func copyModel(t *testing.T, from, to string) {
	t.Helper()
	err := filepath.Walk(from, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(from, path)
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(to, rel), 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(to, rel), data, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}
