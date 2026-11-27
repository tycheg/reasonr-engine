package testrun

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
)

// coordinate is reasonrd test --serve: "model test × drivers" jobs in the
// coordinator queue on --listen; the report comes when the workers return
// the results of every job (the same format as a local run).
func coordinate(ctx context.Context, args, drivers []string, ext string, opts Options, listen, format string, verbose bool,
	stdout, stderr io.Writer) int {
	c := NewCoordinator(2*time.Minute, 2, os.Getenv("REASONR_TEST_TOKEN"))
	if d, err := time.ParseDuration(os.Getenv("REASONR_TEST_LEASE")); err == nil {
		c.TTL = d // for checks of a lost worker
	}
	code := 0
	for _, dir := range args {
		mod, err := reasonr.Load(dir)
		if err != nil {
			fmt.Fprintf(stderr, "reasonr: %v\n", err)
			return 2
		}
		for _, t := range Select(mod, ext) {
			m, err := mod.Build(reasonr.Options{Extension: t, Test: true})
			var errs *reasonr.Errors
			if errors.As(err, &errs) {
				stderr.Write(errs.Text)
				code = 1
				continue
			}
			if err != nil {
				fmt.Fprintf(stderr, "reasonr: %s: %v\n", dir, err)
				return 2
			}
			hash := m.SourceHash()
			for _, name := range m.Tests() {
				if (opts.Run == nil || opts.Run.MatchString(name)) && (opts.Select == nil || opts.Select[name]) {
					c.Add(Job{Module: dir, Model: t, Test: name, Hash: hash, Drivers: drivers})
				}
			}
		}
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		fmt.Fprintf(stderr, "reasonr: --listen: %v\n", err)
		return 2
	}
	srv := &http.Server{Handler: c.Handler()}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()
	fmt.Fprintf(stderr, "coordinator: %s, %d jobs — waiting for workers (reasonrd test-worker --coordinator http://%s)\n",
		ln.Addr(), len(c.jobs), ln.Addr())
	reports, err := c.Wait(ctx)
	// Workers that waited for jobs will ask again: for a second answer "no
	// jobs" so that they finish without an error.
	time.Sleep(time.Second)
	if err != nil {
		fmt.Fprintf(stderr, "reasonr: %v\n", err)
		return 2
	}
	for _, rep := range reports {
		if rep.Failed() > 0 {
			code = 1
		}
		if format == "text" {
			if err := WriteText(stdout, rep, verbose); err != nil {
				return 2
			}
		}
	}
	if format == "json" {
		if err := WriteJSON(stdout, reports, verbose); err != nil {
			return 2
		}
	}
	fmt.Fprintf(stderr, "coordinator: results returned by workers: %d\n", c.Workers())
	return code
}

// WorkerCommand is reasonrd test-worker --coordinator address [--drivers
// memory,api,web] [--slots N] [--url address | --config engine.yaml]
// [--name name]: it takes the coordinator's jobs while there are any.
func WorkerCommand(args []string, stdout, stderr io.Writer, env Env) int {
	fl := flag.NewFlagSet("test-worker", flag.ContinueOnError)
	fl.SetOutput(stderr)
	coord := fl.String("coordinator", "", "the address of the reasonrd test --serve coordinator")
	driverList := fl.String("drivers", "memory,api", "the drivers the worker runs")
	slots := fl.Int("slots", 8, "jobs at once")
	url := fl.String("url", "", "the test mode server for api and web; without it — its own in this process")
	cfg := fl.String("config", "", "the configuration of its own server")
	host, _ := os.Hostname()
	name := fl.String("name", fmt.Sprintf("%s-%d", host, os.Getpid()), "the worker name in the coordinator report")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if *coord == "" {
		fmt.Fprintln(stderr, "reasonrd test-worker: set --coordinator")
		return 2
	}
	drivers, code := parseDrivers(*driverList, env, stderr)
	if code != 0 {
		return code
	}
	if containsString(drivers, "web") {
		defer env.Web.Close()
	}
	err := Work(context.Background(), WorkerOptions{Coordinator: *coord, Token: os.Getenv("REASONR_TEST_TOKEN"), Name: *name,
		Slots: *slots, Drivers: drivers, URL: *url, Config: *cfg, Env: env, Stderr: stderr})
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(stderr, "reasonrd test-worker: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "worker %s: no more jobs\n", strings.TrimSpace(*name))
	return 0
}

func containsString(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
