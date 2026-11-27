package testrun

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"
)

// FreeAddr is a free address on 127.0.0.1 for a server in process.
func FreeAddr() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	return l.Addr().String(), nil
}

// ServeArgs are the reasonrd serve arguments of a test mode server of the
// module module at the address addr; config "" — everything in memory.
func ServeArgs(module, config, addr string) []string {
	args := []string{"serve", "--model", module, "--test", "--listen", addr}
	if config != "" {
		args = append(args, "--config", config)
	}
	return args
}

// WaitReady waits until the server url answers GET /healthz (the server
// listens once the models are loaded; /openapi.json of a large model takes
// long to build); exited closes if the server exits earlier.
func WaitReady(ctx context.Context, url string, exited <-chan struct{}) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/healthz", nil)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-exited:
			return fmt.Errorf("the server exited")
		case <-ctx.Done():
			return fmt.Errorf("the server did not answer: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Log is the log of a server in process: several goroutines write it.
type Log struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *Log) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *Log) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// ExecServer is an Env.Serve that runs the engine binary bin (reasonrd or
// a platform build): it has the configuration's adapters.
func ExecServer(bin string) func(ctx context.Context, module, config string) (string, func() string, error) {
	return func(ctx context.Context, module, config string) (string, func() string, error) {
		addr, err := FreeAddr()
		if err != nil {
			return "", nil, err
		}
		log := &Log{}
		cmd := exec.Command(bin, ServeArgs(module, config, addr)...)
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			return "", nil, err
		}
		exited := make(chan struct{})
		go func() { _ = cmd.Wait(); close(exited) }()
		stop := func() string {
			_ = cmd.Process.Signal(os.Interrupt)
			select {
			case <-exited:
			case <-time.After(10 * time.Second):
				_ = cmd.Process.Kill()
				<-exited
			}
			return log.String()
		}
		url := "http://" + addr
		if err := WaitReady(ctx, url, exited); err != nil {
			return "", nil, fmt.Errorf("%w\n%s", err, stop())
		}
		return url, stop, nil
	}
}
