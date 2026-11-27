package server

import (
	"context"
	"fmt"
	"io"

	"github.com/tycheg/reasonr-engine/internal/registry"
	"github.com/tycheg/reasonr-engine/internal/testrun"
)

// testCommand is reasonrd test (testrun.Command): acceptance tests on the
// memory, api and mcp drivers, and on web when the binary links the web
// driver (registry.RegisterDriver); the server for api and web without
// --url runs in this process, with its own adapters.
func testCommand(args []string, stdout, stderr io.Writer) int {
	return testrun.Command(args, stdout, stderr, testEnv())
}

// testEnv is the environment of the test commands: the server in this
// process and the web driver the binary links.
func testEnv() testrun.Env {
	return testrun.Env{Serve: serveTest, Web: registry.Driver("web")}
}

// serveTest is testrun.Env.Serve: reasonrd serve --test of the module in
// this process on a free address.
func serveTest(ctx context.Context, module, config string) (string, func() string, error) {
	addr, err := testrun.FreeAddr()
	if err != nil {
		return "", nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	log := &testrun.Log{}
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		serve(ctx, testrun.ServeArgs(module, config, addr)[1:], log, nil)
	}()
	stop := func() string {
		cancel()
		<-exited
		return log.String()
	}
	url := "http://" + addr
	if err := testrun.WaitReady(ctx, url, exited); err != nil {
		return "", nil, fmt.Errorf("%w\n%s", err, stop())
	}
	return url, stop, nil
}

// workerCommand is reasonrd test-worker (testrun.WorkerCommand).
func workerCommand(args []string, stdout, stderr io.Writer) int {
	return testrun.WorkerCommand(args, stdout, stderr, testEnv())
}
