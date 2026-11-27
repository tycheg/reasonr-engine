package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tycheg/reasonr-engine/internal/testmodels"
	"github.com/tycheg/reasonr-engine/internal/testrun"
)

// TestWorkers: a run through a coordinator and three workers gives the same
// report as a local run, on the memory and api drivers; a worker that took
// jobs and disappeared loses no tests — once the lease expires, others run
// them.
func TestWorkers(t *testing.T) {
	if testing.Short() {
		t.Skip("workers — not with -short")
	}
	for _, name := range []string{"taxi", "bank"} {
		t.Run(name, func(t *testing.T) {
			if name == "taxi" {
				t.Setenv("REASONR_MODULES", testmodels.Modules()) // module Auth
			}
			testWorkers(t, testmodels.Dir(name))
		})
	}
}

func testWorkers(t *testing.T, model string) {
	var local, localErr bytes.Buffer
	if code := testCommand([]string{"--driver", "memory,api", "--format", "json", model}, &local, &localErr); code != 0 {
		t.Fatalf("local run: code %d\n%s", code, localErr.String())
	}
	want := summary(t, local.Bytes())

	run := func(lost bool) map[string]bool {
		t.Setenv("REASONR_TEST_LEASE", "2s")
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		ln.Close()
		var out, errOut bytes.Buffer
		done := make(chan int, 1)
		go func() {
			done <- testrun.Command([]string{"--serve", "--listen", addr, "--driver", "memory,api", "--format", "json", model},
				&out, &errOut, testrun.Env{Serve: serveTest})
		}()
		coord := "http://" + addr
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			if resp, err := http.Post(coord+"/heartbeat", "application/json", strings.NewReader(`{"jobs":[]}`)); err == nil {
				resp.Body.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the coordinator did not start: %s", errOut.String())
			}
		}
		if lost { // a worker takes five jobs and disappears
			for range 5 {
				resp, err := http.Post(coord+"/lease", "application/json", strings.NewReader(`{"worker":"lost","drivers":["memory","api"]}`))
				if err != nil || resp.StatusCode != http.StatusOK {
					t.Fatalf("lease: %v", err)
				}
				resp.Body.Close()
			}
		}
		var wg sync.WaitGroup
		for i := range 3 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := testrun.Work(context.Background(), testrun.WorkerOptions{Coordinator: coord, Name: fmt.Sprintf("w%d", i),
					Slots: 2, Drivers: []string{"memory", "api"}, Env: testrun.Env{Serve: serveTest}})
				if err != nil {
					t.Errorf("worker %d: %v", i, err)
				}
			}()
		}
		wg.Wait()
		if code := <-done; code != 0 {
			t.Fatalf("coordinator: code %d\n%s", code, errOut.String())
		}
		if !strings.Contains(errOut.String(), "results returned by workers: 3") {
			t.Errorf("not all workers worked: %s", errOut.String())
		}
		return summary(t, out.Bytes())
	}
	for _, lost := range []bool{false, true} {
		got := run(lost)
		if fmt.Sprint(sorted(got)) != fmt.Sprint(sorted(want)) {
			t.Errorf("lost worker %v: the report differs from the local one\nlocal:   %v\nworkers: %v", lost, sorted(want), sorted(got))
		}
	}
}

// summary is "module [extension] test" → passed.
func summary(t *testing.T, data []byte) map[string]bool {
	var rep struct {
		Models []struct {
			Module, Extension string
			Tests             []struct {
				Name   string
				Passed bool
			}
		}
	}
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatalf("report: %v\n%s", err, data)
	}
	out := map[string]bool{}
	for _, m := range rep.Models {
		for _, tt := range m.Tests {
			out[m.Module+" ["+m.Extension+"] "+tt.Name] = tt.Passed
		}
	}
	return out
}

func sorted(m map[string]bool) []string {
	var out []string
	for k, v := range m {
		out = append(out, fmt.Sprintf("%s=%v", k, v))
	}
	sort.Strings(out)
	return out
}
