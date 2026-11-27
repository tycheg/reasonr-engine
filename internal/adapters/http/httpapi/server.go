// Package httpapi is the HTTP transport of the ReasonR engine (RFC §31.3,
// §30–31): the model API by schema (commands, queries, canonical JSON),
// notifications to other actors over WebSocket and long polling, the
// OpenAPI of the tenant model and of the integration service contract,
// /healthz, /readyz, /metrics and request logs. The transport depends only
// on port.Executor: it does not know where commands run.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/telemetry"
	"github.com/tycheg/reasonr-engine/internal/codecs"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Options are the transport settings.
type Options struct {
	Addr string // the Serve address, for example ":8080"
	// Codecs are the codecs of the model API besides json (ingress[].codecs).
	Codecs      []port.Codec
	LogRequests bool // log every request
	LongPoll    bool // switch on /api/poll
	// Ready reports readiness for /readyz; nil — always ready.
	Ready func(ctx context.Context) error
	// Metrics serves /metrics; nil — no endpoint.
	Metrics http.Handler
	// MaxBody is the largest request body; 0 — 1 MB.
	MaxBody int64
	// PingInterval is the WebSocket ping; 0 — 30 s.
	PingInterval time.Duration
	// Mount holds extra handlers by path prefix (the test API /_test/).
	Mount map[string]http.Handler
	// MCP is the model's MCP server at POST /mcp (RFC §31.3.2).
	MCP bool
	// Issuers are the authorization servers for the MCP protected resource
	// metadata (/.well-known/oauth-protected-resource, RFC 9728).
	Issuers []string
	// AllPublic opens every public method in /api (test mode); otherwise
	// only those published in the external API contract (RFC §31.3.1).
	AllPublic bool
	// TimeOffset reports whether a tenant accepts the Reasonr-Time-Offset
	// header (RFC §27.5): every tenant in test mode, otherwise the demo
	// tenants; nil — none.
	TimeOffset func(port.Tenant) bool
	// Handlers are handlers that need an executor: built at startup (the
	// app API /app/, adapters/http/appapi). The key is the path pattern.
	Handlers map[string]func(x kind.Executor, deps ports.EdgeDeps) http.Handler
}

// TimeOffsetHeader is the scenario clock offset: "-30d", "+20s".
const TimeOffsetHeader = "Reasonr-Time-Offset"

// Server is the HTTP API over an executor.
type Server struct {
	x    kind.Executor
	auth ports.Authenticator
	log  *slog.Logger
	opts Options

	requests port.Counter
	latency  port.Histogram
	apiCalls port.Counter // external API calls by method and version
	views    views        // schemas as API versions see them
	apiOnce  sync.Once
	apiMux   *http.ServeMux // /api — for MCP tool calls

	mu     sync.Mutex
	codecs map[*reasonr.Schema]*codecs.Values
	polls  *polls
	deps   ports.EdgeDeps
}

var _ ports.Listener = (*Listener)(nil)

// New returns the API server of the executor x.
func New(x kind.Executor, deps ports.EdgeDeps, opts Options) *Server {
	if opts.MaxBody == 0 {
		opts.MaxBody = 1 << 20
	}
	if opts.PingInterval == 0 {
		opts.PingInterval = 30 * time.Second
	}
	s := &Server{x: x, auth: deps.Auth, log: deps.Logger, opts: opts, codecs: map[*reasonr.Schema]*codecs.Values{}, deps: deps}
	if s.log == nil {
		s.log = telemetry.Discard()
	}
	m := deps.Metrics
	if m == nil {
		m = ports.Nop{}
	}
	s.requests = m.Counter("reasonr_http_requests_total", "HTTP requests by route and response code.", "route", "code")
	s.latency = m.Histogram("reasonr_http_request_seconds", "HTTP response time.", nil, "route")
	s.apiCalls = m.Counter("reasonr_api_calls_total", "External API calls by method and client version (RFC §31.3.1).", "method", "version")
	s.polls = newPolls(s)
	return s
}

// apiHandler holds the /api routes (MCP tool calls).
func (s *Server) apiHandler() http.Handler {
	s.apiOnce.Do(func() {
		s.apiMux = http.NewServeMux()
		s.apiMux.HandleFunc("POST /api/{type}/{id}/{method}", s.route("command", s.command))
		s.apiMux.HandleFunc("POST /api/{type}/{method}", s.route("command", s.command))
		s.apiMux.HandleFunc("GET /api/{type}/{method}", s.route("query", s.query))
	})
	return s.apiMux
}

// Handler returns the handler of every route.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	if s.opts.MCP {
		mux.HandleFunc("POST /mcp", s.route("mcp", s.mcp))
		mux.HandleFunc("GET /.well-known/oauth-protected-resource", s.protectedResource)
		mux.HandleFunc("GET /mcp", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Allow", "POST")
			http.Error(w, "MCP: POST only — the server opens no event stream", http.StatusMethodNotAllowed)
		})
	}
	mux.HandleFunc("POST /api/{type}/{id}/{method}", s.route("command", s.command))
	mux.HandleFunc("POST /api/{type}/{method}", s.route("command", s.command)) // a create command
	mux.HandleFunc("GET /api/{type}/{method}", s.route("query", s.query))
	mux.HandleFunc("GET /api/ws", s.route("ws", s.websocket))
	if s.opts.LongPoll {
		mux.HandleFunc("GET /api/poll", s.route("poll", s.poll))
	}
	mux.HandleFunc("GET /openapi.json", s.route("openapi", s.openapi))
	mux.HandleFunc("GET /openapi/integrations.json", s.route("openapi", s.integrationsOpenAPI))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if s.opts.Ready != nil {
			if err := s.opts.Ready(r.Context()); err != nil {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
				return
			}
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	if s.opts.Metrics != nil {
		mux.Handle("GET /metrics", s.opts.Metrics)
	}
	for prefix, h := range s.opts.Mount {
		mux.Handle(prefix, h)
	}
	for pattern, f := range s.opts.Handlers {
		mux.Handle(pattern, f(s.x, s.deps))
	}
	return mux
}

// codec returns the schema's canonical JSON.
func (s *Server) codec(sc *reasonr.Schema) *codecs.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.codecs[sc]
	if c == nil {
		c = codecs.JSONOf(sc, codecs.Options{})
		s.codecs[sc] = c
	}
	return c
}

// call is the request state for the log and metrics: the handler fills it.
type call struct {
	tenant   port.Tenant
	caller   string
	position port.Position
	incident string
	status   int
}

type statusWriter struct {
	http.ResponseWriter
	c *call
}

func (w *statusWriter) WriteHeader(code int) {
	if w.c.status == 0 {
		w.c.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.c.status == 0 {
		w.c.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Unwrap gives http.ResponseController access to the original writer
// (Hijack for WebSocket, Flush).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// route wraps a handler: metrics, the request log.
func (s *Server) route(name string, h func(http.ResponseWriter, *http.Request, *call)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		c := &call{}
		if v := r.Header.Get(TimeOffsetHeader); v != "" {
			// The scenario clock offset (RFC §27.5): authenticate checks the tenant.
			d, err := ports.ParseOffset(v)
			if err != nil {
				http.Error(w, TimeOffsetHeader+": "+err.Error(), http.StatusBadRequest)
				return
			}
			r = r.WithContext(port.WithOffset(r.Context(), d))
		}
		h(&statusWriter{ResponseWriter: w, c: c}, r, c)
		if c.status == 0 {
			c.status = http.StatusOK
		}
		d := time.Since(start)
		s.requests.Add(1, name, statusText(c.status))
		s.latency.Observe(d.Seconds(), name)
		if s.opts.LogRequests || c.incident != "" {
			attrs := []any{"method", r.Method, "path", r.URL.Path, "status", c.status, "duration", d}
			if c.tenant != "" {
				attrs = append(attrs, "tenant", string(c.tenant))
			}
			if c.caller != "" {
				attrs = append(attrs, "caller", c.caller)
			}
			if c.position != 0 {
				attrs = append(attrs, "position", uint64(c.position))
			}
			if c.incident != "" {
				attrs = append(attrs, "incident", c.incident)
			}
			s.log.Info("request", attrs...)
		}
	}
}

func statusText(code int) string {
	b := [3]byte{byte('0' + code/100%10), byte('0' + code/10%10), byte('0' + code%10)}
	return string(b[:])
}

// fail answers 500 and logs the fault with an incident id: the details
// (the place in the model, the stack) go only to the log.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, c *call, err error) {
	var b [8]byte
	_, _ = rand.Read(b[:])
	c.incident = hex.EncodeToString(b[:])
	s.log.Error("execution fault", "incident", c.incident, "method", r.Method, "path", r.URL.Path,
		"tenant", string(c.tenant), "error", err)
	writeJSON(w, http.StatusInternalServerError, []byte(`{"status":"failed","error":"internalError","incident":"`+c.incident+`"}`))
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// Listener is the HTTP transport as a ports.Listener: Serve listens on
// Options.Addr until ctx is cancelled, then stops gracefully.
type Listener struct {
	Options Options
	// Listen is an already open socket; nil — net.Listen("tcp", Options.Addr).
	Listen net.Listener
}

// Serve serves the API of the executor x until ctx is cancelled.
func (l *Listener) Serve(ctx context.Context, x kind.Executor, deps ports.EdgeDeps) error {
	s := New(x, deps, l.Options)
	ln := l.Listen
	if ln == nil {
		var err error
		if ln, err = net.Listen("tcp", l.Options.Addr); err != nil {
			return err
		}
	}
	srv := &http.Server{Handler: s.Handler(), BaseContext: func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	shut, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := srv.Shutdown(shut)
	s.polls.close()
	if e := <-done; !errors.Is(e, http.ErrServerClosed) {
		err = errors.Join(err, e)
	}
	return err
}
