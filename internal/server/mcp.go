package server

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// mcpCommand is reasonrd mcp --stdio --as caller [--config file] [module]:
// the model's MCP server for an agent on a developer machine (RFC §31.3.2).
// The engine is as in reasonrd dev (test mode, in memory without
// --config); the agent's JSON-RPC messages go line by line from stdin to
// its POST /mcp from the caller --as (actor=Customer:<id> or role=admin),
// the responses line by line to stdout. The engine log goes to stderr.
func mcpCommand(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fl := flag.NewFlagSet("reasonrd mcp", flag.ContinueOnError)
	fl.SetOutput(stderr)
	stdio := fl.Bool("stdio", false, "the stdio transport: JSON-RPC line by line on stdin and stdout")
	as := fl.String("as", "", "the caller: actor=<Actor>:<id> or role=<role> (tenant=<tenant>)")
	cfg := fl.String("config", "", "the engine configuration; by default everything is in memory")
	if err := fl.Parse(args); err != nil {
		return 2
	}
	if !*stdio || *as == "" || fl.NArg() > 1 {
		fmt.Fprintln(stderr, "reasonrd mcp --stdio --as actor=<Actor>:<id> [--config file] [module]")
		return 2
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(stderr, "reasonrd: %v\n", err)
		return 1
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	devArgs := []string{"--listen", addr, "--watch", "0"}
	if *cfg != "" {
		devArgs = append(devArgs, "--config", *cfg)
	}
	devArgs = append(devArgs, fl.Args()...)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan int, 1)
	go func() { done <- dev(ctx, devArgs, stderr, nil) }()
	base := "http://" + addr
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if resp, err := http.Get(base + "/healthz"); err == nil {
			resp.Body.Close()
			break
		}
		select {
		case code := <-done:
			return code
		default:
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(stderr, "reasonrd mcp: the engine did not start")
			return 1
		}
	}
	session := ""
	in := bufio.NewScanner(stdin)
	in.Buffer(make([]byte, 1<<20), 16<<20)
	for in.Scan() {
		line := bytes.TrimSpace(in.Bytes())
		if len(line) == 0 {
			continue
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, base+"/mcp", bytes.NewReader(line))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Test "+strings.TrimSpace(*as))
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Fprintf(stderr, "reasonrd mcp: %v\n", err)
			return 1
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
			session = sid
		}
		if resp.StatusCode == http.StatusAccepted || len(bytes.TrimSpace(body)) == 0 {
			continue // a client notification — no response
		}
		fmt.Fprintln(stdout, string(bytes.TrimSpace(body)))
	}
	cancel()
	<-done
	return 0
}
