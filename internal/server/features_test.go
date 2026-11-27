package server

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tycheg/reasonr-engine/internal/host"
	"github.com/tycheg/reasonr-engine/internal/testrun"
)

const tipsModel = `role admin

## Tips for the driver
feature tips "2026-11-07"

aggregate Order
  id uuid
  tip int

  error tipsDisabled()

  command(admin) LeaveTip(amount int)
    if tips
      tipped(amount: amount)
    else
      tipsDisabled()

  event tipped(amount int)
    tip = amount

test "Tips"
  feature tips
  let o1 = new Order.Id
  when
    [o1].LeaveTip(amount: 10) as admin

test "Without the flag"
  let o1 = new Order.Id
  when
    [o1].LeaveTip(amount: 10) as admin
  then
    error tipsDisabled
`

// TestFeatureDev: reasonrd feature on|off|list|log --url rolls a flag out in
// the reasonrd dev engine without a restart (RFC §23.4).
func TestFeatureDev(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "m.rr"), []byte(tipsModel), 0o644); err != nil {
		t.Fatal(err)
	}
	addr, err := testrun.FreeAddr()
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + addr
	ctx, cancel := context.WithCancel(context.Background())
	var log syncBuffer
	started := make(chan struct{}, 1)
	done := make(chan int, 1)
	go func() {
		done <- runWith(ctx, []string{"dev", "--listen", addr, dir}, os.Stdout, &log, func(*host.Engine) { started <- struct{}{} })
	}()
	defer func() {
		cancel()
		<-done
		if t.Failed() {
			t.Log(log.String())
		}
	}()
	select {
	case <-started:
	case <-time.After(15 * time.Second):
		t.Fatal("the engine did not start")
	}
	call := func() int {
		t.Helper()
		req, _ := http.NewRequest("POST", base+"/api/Order/00000000-0000-0000-0000-0000000000a1/LeaveTip", strings.NewReader(`{"amount": 5}`))
		req.Header.Set("Authorization", "Test role=admin")
		var resp *http.Response
		for range 50 { // the HTTP ingress starts together with the roles
			if resp, err = http.DefaultClient.Do(req); err == nil {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	feature := func(args ...string) string {
		t.Helper()
		var out, errs bytes.Buffer
		if code := Run(ctx, append([]string{"feature"}, append(args, "--url", base)...), &out, &errs); code != 0 {
			t.Fatalf("reasonrd feature %v: code %d: %s", args, code, errs.String())
		}
		return out.String()
	}
	if got := call(); got != http.StatusUnprocessableEntity {
		t.Fatalf("the flag is off: %d", got)
	}
	feature("on", "tips", "--role", "admin", "--tenant", "default")
	if got := call(); got != http.StatusOK {
		t.Fatalf("the flag is on for the role: %d", got)
	}
	if list := feature("list"); !strings.Contains(list, "role=admin tenant=default") || !strings.Contains(list, "2026-11-07") {
		t.Errorf("feature list:\n%s", list)
	}
	feature("off", "tips")
	if got := call(); got != http.StatusUnprocessableEntity {
		t.Fatalf("the flag is off everywhere: %d", got)
	}
	if l := feature("log", "tips"); strings.Count(l, "tips") != 2 || !strings.Contains(l, "off at every level") {
		t.Errorf("feature log:\n%s", l)
	}
	var errs bytes.Buffer
	if code := Run(ctx, []string{"feature", "on", "nope", "--all", "--url", base}, &bytes.Buffer{}, &errs); code != 1 {
		t.Errorf("unknown flag: code %d: %s", code, errs.String())
	}
}

// TestFeatureTest: reasonrd test switches flags on with a feature line on
// the memory and api drivers.
func TestFeatureTest(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "m.rr"), []byte(tipsModel), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errs bytes.Buffer
	if code := Run(context.Background(), []string{"test", "--driver", "memory,api", dir}, &out, &errs); code != 0 {
		t.Fatalf("code %d:\n%s\n%s", code, out.String(), errs.String())
	}
}
