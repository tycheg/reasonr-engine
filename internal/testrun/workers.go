package testrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
)

// Test run workers: the coordinator keeps a queue of "model test" jobs and
// hands them to workers that pull them with a lease; a lost worker loses no
// tests — once the lease expires the job goes back to the queue, and after
// Retries repeats it fails with fault. The protocol is HTTP/JSON with a
// shared token (REASONR_TEST_TOKEN):
//
//	POST /lease      {"worker": "…", "drivers": ["memory","api"]} → 200 Job | 202 — wait | 204 — no more jobs
//	POST /heartbeat  {"jobs": [1, 2]} — extend the leases
//	POST /result     {"job": 1, "test": {…}} — a test result (as in the report)
//
// A worker builds the program itself from its own copy of the module (in
// CI — the same repository) and checks the source hash against the job.

// Job is a job: a model test of a module on drivers.
type Job struct {
	ID      int      `json:"id"`
	Module  string   `json:"module"` // the module path as given to the coordinator
	Model   string   `json:"model"`  // "" — the base model
	Test    string   `json:"test"`
	Hash    string   `json:"hash"` // the hash of the model sources
	Drivers []string `json:"drivers"`
}

// Coordinator is the job queue and the report assembly.
type Coordinator struct {
	// TTL is the job lease (heartbeat extends it); Retries — repeats after
	// a lost worker, then a fault.
	TTL     time.Duration
	Retries int
	Token   string

	mu       sync.Mutex
	jobs     []*Job
	pending  []int // the job numbers in the queue
	leases   map[int]lease
	attempts map[int]int
	results  map[int]*Test
	workers  map[int]string // who returned a result
	done     chan struct{}
	closed   bool
}

type lease struct {
	worker string
	until  time.Time
}

// NewCoordinator creates a coordinator with the lease ttl.
func NewCoordinator(ttl time.Duration, retries int, token string) *Coordinator {
	return &Coordinator{TTL: ttl, Retries: retries, Token: token, leases: map[int]lease{}, attempts: map[int]int{},
		results: map[int]*Test{}, workers: map[int]string{}, done: make(chan struct{})}
}

// Add queues a job.
func (c *Coordinator) Add(j Job) {
	c.mu.Lock()
	defer c.mu.Unlock()
	j.ID = len(c.jobs)
	c.jobs = append(c.jobs, &j)
	c.pending = append(c.pending, j.ID)
}

// Handler holds the routes of the worker protocol.
func (c *Coordinator) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /lease", c.lease)
	mux.HandleFunc("POST /heartbeat", c.heartbeat)
	mux.HandleFunc("POST /result", c.result)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c.Token != "" && r.Header.Get("Authorization") != "Bearer "+c.Token {
			http.Error(w, "the REASONR_TEST_TOKEN token is needed", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (c *Coordinator) lease(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Worker  string
		Drivers []string
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reap()
	for i, id := range c.pending {
		j := c.jobs[id]
		if !subset(j.Drivers, req.Drivers) {
			continue
		}
		c.pending = slices.Delete(c.pending, i, i+1)
		c.leases[id] = lease{worker: req.Worker, until: time.Now().Add(c.TTL)}
		c.attempts[id]++
		writeJSONReply(w, j)
		return
	}
	if len(c.pending) == 0 && len(c.leases) == 0 {
		w.WriteHeader(http.StatusNoContent) // the run is over
		return
	}
	w.WriteHeader(http.StatusAccepted) // there are jobs, but not now or not for these drivers
}

func subset(need, have []string) bool {
	for _, d := range need {
		if !slices.Contains(have, d) {
			return false
		}
	}
	return true
}

func (c *Coordinator) heartbeat(w http.ResponseWriter, r *http.Request) {
	var req struct{ Jobs []int }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range req.Jobs {
		if l, ok := c.leases[id]; ok {
			l.until = time.Now().Add(c.TTL)
			c.leases[id] = l
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *Coordinator) result(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Job    int
		Worker string
		Test   *Test
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Test == nil || req.Test.TestResult == nil {
		http.Error(w, "result: expected {job, worker, test}", http.StatusBadRequest)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.results[req.Job]; !ok && req.Job >= 0 && req.Job < len(c.jobs) {
		c.results[req.Job] = req.Test
		c.workers[req.Job] = req.Worker
		delete(c.leases, req.Job)
		c.pending = slices.DeleteFunc(c.pending, func(id int) bool { return id == req.Job }) // a late result after a repeat
		c.finish()
	}
	w.WriteHeader(http.StatusNoContent)
}

// reap returns jobs with an expired lease to the queue; after Retries
// repeats — a fault. Under the lock.
func (c *Coordinator) reap() {
	now := time.Now()
	for id, l := range c.leases {
		if now.Before(l.until) {
			continue
		}
		delete(c.leases, id)
		if c.attempts[id] > c.Retries {
			j := c.jobs[id]
			c.results[id] = &Test{TestResult: &reasonr.TestResult{Name: j.Test, Failures: []reasonr.Failure{{Kind: reasonr.FailFault,
				Message: fmt.Sprintf("the worker returned no result: %d attempts, the last one — %s", c.attempts[id], l.worker)}}}}
			continue
		}
		c.pending = append(c.pending, id)
	}
	c.finish()
}

// finish closes done once every job has a result. Under the lock.
func (c *Coordinator) finish() {
	if !c.closed && len(c.results) == len(c.jobs) {
		c.closed = true
		close(c.done)
	}
}

// Wait waits for the results of every job and assembles the reports by
// model in queue order; ctx interrupts it.
func (c *Coordinator) Wait(ctx context.Context) ([]*Report, error) {
	tick := time.NewTicker(max(c.TTL/4, 10*time.Millisecond))
	defer tick.Stop()
	for {
		c.mu.Lock()
		c.reap()
		c.mu.Unlock()
		select {
		case <-c.done:
			return c.reports(), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
}

func (c *Coordinator) reports() []*Report {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*Report
	byKey := map[string]*Report{}
	for _, j := range c.jobs {
		key := j.Module + "\x00" + j.Model
		rep := byKey[key]
		if rep == nil {
			rep = &Report{Module: j.Module, Extension: j.Model}
			byKey[key] = rep
			out = append(out, rep)
		}
		rep.Tests = append(rep.Tests, *c.results[j.ID])
	}
	return out
}

// Workers returns the number of distinct workers that returned results.
func (c *Coordinator) Workers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := map[string]bool{}
	for _, w := range c.workers {
		seen[w] = true
	}
	return len(seen)
}

func writeJSONReply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// WorkerOptions configure a run worker.
type WorkerOptions struct {
	Coordinator string
	Token       string
	Name        string
	Slots       int      // jobs at once
	Drivers     []string // the drivers the worker runs
	URL         string   // the test mode server for api and web; "" — its own (Env.Serve)
	Config      string   // the configuration of its own server
	Env         Env
	Stderr      io.Writer
}

// Work takes and runs jobs until the coordinator says there are no more.
func Work(ctx context.Context, o WorkerOptions) error {
	w := &worker{o: o, client: &http.Client{Timeout: 30 * time.Second}, models: map[string]*workerModel{}}
	defer w.close()
	errs := make(chan error, max(o.Slots, 1))
	var wg sync.WaitGroup
	for range max(o.Slots, 1) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := w.loop(ctx); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	return <-errs
}

type worker struct {
	o      WorkerOptions
	client *http.Client
	mu     sync.Mutex
	models map[string]*workerModel
	stops  []func() string
}

// workerModel is the built model of a job and its drivers.
type workerModel struct {
	once    sync.Once
	m       *reasonr.Model
	prep    *prepared
	drivers []Driver
	err     error
}

func (w *worker) close() {
	for _, stop := range w.stops {
		stop()
	}
}

func (w *worker) post(ctx context.Context, path string, body any) (int, []byte, error) {
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(w.o.Coordinator, "/")+path, bytes.NewReader(data))
	if err != nil {
		return 0, nil, err
	}
	if w.o.Token != "" {
		req.Header.Set("Authorization", "Bearer "+w.o.Token)
	}
	resp, err := w.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	return resp.StatusCode, out, err
}

func (w *worker) loop(ctx context.Context) error {
	worked := false
	for ctx.Err() == nil {
		status, data, err := w.post(ctx, "/lease", map[string]any{"worker": w.o.Name, "drivers": w.o.Drivers})
		switch {
		case err != nil && worked:
			return nil // the coordinator collected the results and closed
		case err != nil:
			return fmt.Errorf("coordinator: %w", err)
		case status == http.StatusNoContent:
			return nil
		case status == http.StatusAccepted:
			time.Sleep(100 * time.Millisecond)
			continue
		case status != http.StatusOK:
			return fmt.Errorf("coordinator: %d %s", status, strings.TrimSpace(string(data)))
		}
		var j Job
		if err := json.Unmarshal(data, &j); err != nil {
			return err
		}
		if wm := w.model(ctx, &j); wm.err != nil {
			// The model did not build or its own server did not start: the
			// worker exits, and others run the job once the lease expires.
			return fmt.Errorf("%s: %w", j.Module, wm.err)
		}
		t, err := w.run(ctx, &j)
		if err != nil {
			t = &Test{TestResult: &reasonr.TestResult{Name: j.Test, Failures: []reasonr.Failure{{Kind: reasonr.FailFault, Message: "worker " + w.o.Name + ": " + err.Error()}}}}
		}
		if _, _, err := w.post(ctx, "/result", map[string]any{"job": j.ID, "worker": w.o.Name, "test": t}); err != nil {
			return fmt.Errorf("coordinator: %w", err)
		}
		worked = true
	}
	return ctx.Err()
}

// run runs a job; heartbeat extends the lease.
func (w *worker) run(ctx context.Context, j *Job) (*Test, error) {
	stopBeat := make(chan struct{})
	defer close(stopBeat)
	go func() {
		ttl := time.NewTicker(2 * time.Second)
		defer ttl.Stop()
		for {
			select {
			case <-stopBeat:
				return
			case <-ttl.C:
				_, _, _ = w.post(ctx, "/heartbeat", map[string]any{"jobs": []int{j.ID}})
			}
		}
	}()
	wm := w.model(ctx, j)
	if wm.err != nil {
		return nil, wm.err
	}
	if h := wm.m.SourceHash(); h != j.Hash {
		return nil, fmt.Errorf("%s: the worker's sources (%s) differ from the coordinator's (%s)", j.Module, h, j.Hash)
	}
	rep, err := Run(ctx, wm.m, Options{prepared: wm.prep, Drivers: wm.drivers, Select: map[string]bool{j.Test: true}})
	if err != nil {
		return nil, err
	}
	if len(rep.Tests) != 1 {
		return nil, fmt.Errorf("no test %q", j.Test)
	}
	return &rep.Tests[0], nil
}

// model builds a job's model once per worker.
func (w *worker) model(ctx context.Context, j *Job) *workerModel {
	key := j.Module + "\x00" + j.Model + "\x00" + strings.Join(j.Drivers, ",")
	w.mu.Lock()
	wm := w.models[key]
	if wm == nil {
		wm = &workerModel{}
		w.models[key] = wm
	}
	w.mu.Unlock()
	wm.once.Do(func() {
		if wm.m, wm.err = reasonr.Build(j.Module, reasonr.Options{Extension: j.Model, Test: true}); wm.err != nil {
			if errors.As(wm.err, new(*reasonr.Errors)) {
				wm.err = fmt.Errorf("%s: the model has errors", j.Module)
			}
			return
		}
		wm.prep = prepare(wm.m)
		url := w.o.URL
		if url == "" && slices.ContainsFunc(j.Drivers, func(d string) bool { return d != "memory" }) {
			if w.o.Env.Serve == nil {
				wm.err = fmt.Errorf("driver %v needs --url", j.Drivers)
				return
			}
			var stop func() string
			if url, stop, wm.err = w.o.Env.Serve(context.WithoutCancel(ctx), j.Module, w.o.Config); wm.err != nil {
				return
			}
			w.mu.Lock()
			w.stops = append(w.stops, stop)
			w.mu.Unlock()
		}
		model := j.Model
		if model == "" {
			model = Base
		}
		var failed bool
		wm.drivers, _, failed = modelDrivers(ctx, wm.m, model, j.Drivers, url, w.o.Env, io.Discard, j.Module)
		if failed {
			wm.err = fmt.Errorf("%s: drivers %v", j.Module, j.Drivers)
		}
	})
	return wm
}
