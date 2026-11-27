// Package appapi is the app API (RFC §31.4, §22): what the platform runtime
// works with. The manifest and screen descriptions, screen data in one
// request, place queries and commands — only those the app has, the web
// runtime build. The model is not in the API: everything the runtime needs
// is in the screen descriptions; methods are called through the executor,
// as in the model API.
package appapi

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/auth"
	"github.com/tycheg/reasonr-engine/internal/adapters/telemetry"
	"github.com/tycheg/reasonr-engine/internal/codecs"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Options are the app API settings.
type Options struct {
	Catalog ports.UICatalog
	// Test is the engine's test mode: "sign in as".
	Test bool
	// Dir is the runtime build directory (ingress.ui.dir); "" — Bundle, or
	// the default bundle a binary links (registry.RegisterUIBundle).
	Dir    string
	Bundle fs.FS
	// Proxy is the address of a runtime dev server (ingress.ui.proxy,
	// reasonrd dev --ui http://localhost:5173): /ui/ is proxied to it
	// instead of Dir and Bundle; "" — none.
	Proxy string
	// OIDC is sign-in through OIDC (ingress.ui.auth.oidc); nil — not configured.
	OIDC *OIDC
	// MaxBody is the largest request body; 0 — 1 MB.
	MaxBody int64
	// Now is the clock for layout expressions; nil — time.Now.
	Now func() time.Time
}

// OIDC holds the sign-in settings: authorization code and PKCE.
type OIDC struct {
	Issuer   string   `json:"issuer"`
	ClientID string   `json:"clientId"`
	Scopes   []string `json:"scopes,omitempty"`
}

// Server is the app API over an executor.
type Server struct {
	x    kind.Executor
	auth ports.Authenticator
	log  *slog.Logger
	opts Options

	mu     sync.Mutex
	codecs map[*reasonr.Schema]*codecs.Values
	views  map[viewKey]*view // screen descriptions in JSON
}

type viewKey struct {
	ui                *reasonr.UI
	app, screen, lang string
}

// New returns the app API of the executor x.
func New(x kind.Executor, deps ports.EdgeDeps, opts Options) *Server {
	if opts.MaxBody == 0 {
		opts.MaxBody = 1 << 20
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	s := &Server{x: x, auth: deps.Auth, log: deps.Logger, opts: opts,
		codecs: map[*reasonr.Schema]*codecs.Values{}, views: map[viewKey]*view{}}
	if s.log == nil {
		s.log = telemetry.Discard()
	}
	return s
}

// Handler returns the /app/ and /ui/ handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /app/_login", s.login)
	mux.HandleFunc("GET /app/{app}", s.withApp(s.manifest))
	mux.HandleFunc("GET /app/{app}/screens/{file}", s.withApp(s.screen))
	mux.HandleFunc("GET /app/{app}/openapi.json", s.withApp(s.openapi))
	mux.HandleFunc("GET /app/{app}/{screen}", s.withApp(s.data))
	mux.HandleFunc("GET /app/{app}/{screen}/{node}", s.withApp(s.placeQuery))
	mux.HandleFunc("POST /app/{app}/{screen}/{node}", s.withApp(s.placeCommand))
	mux.Handle("GET /ui/", s.static())
	return mux
}

// login is GET /app/_login: how to sign in (before authentication): test
// mode "sign in as" or OIDC.
func (s *Server) login(w http.ResponseWriter, _ *http.Request) {
	body := struct {
		Test bool  `json:"test,omitempty"`
		OIDC *OIDC `json:"oidc,omitempty"`
	}{s.opts.Test, s.opts.OIDC}
	data, _ := json.Marshal(body)
	writeJSON(w, http.StatusOK, data)
}

// req is a request to an app after authentication.
type req struct {
	tenant port.Tenant
	caller reasonr.Caller
	sc     *reasonr.Schema
	codec  *codecs.Values
	model  *reasonr.Model
	app    *reasonr.UIApp
	a      *reasonr.Arena
}

// withApp authenticates the caller, finds the model and the app and checks
// that the caller is admitted to it.
func (s *Server) withApp(h func(w http.ResponseWriter, r *http.Request, q *req)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a := reasonr.GetArena()
		defer reasonr.PutArena(a)
		q := &req{a: a}
		hdr := r.Header.Get("Authorization")
		if hdr == "" || s.auth == nil {
			unauthorized(w, "no credentials")
			return
		}
		cred := auth.ParseAuthorization(hdr)
		cred.Tenant = port.Tenant(r.Header.Get("X-Reasonr-Tenant"))
		var err error
		if q.tenant, q.caller, err = s.auth.Authenticate(r.Context(), cred, a); err != nil {
			if errors.Is(err, ports.ErrForbidden) {
				http.Error(w, err.Error(), http.StatusForbidden)
			} else {
				unauthorized(w, err.Error())
			}
			return
		}
		if q.sc, err = s.x.Schema(q.tenant); err != nil {
			unauthorized(w, err.Error())
			return
		}
		if s.opts.Catalog != nil {
			q.model = s.opts.Catalog.UI(q.sc)
		}
		if q.model == nil || q.model.UI().App(r.PathValue("app")) == nil {
			http.NotFound(w, r)
			return
		}
		q.app = q.model.UI().App(r.PathValue("app"))
		if !enters(q.sc, q.app, q.caller) {
			http.Error(w, "the caller is not admitted to app "+q.app.Name, http.StatusForbidden)
			return
		}
		if fx, ok := s.x.(ports.FeatureExecutor); ok && fx.Features() != nil {
			q.caller.Features = fx.Features().Features(q.tenant, q.sc, &q.caller, a)
		}
		q.codec = s.codec(q.sc)
		h(w, r, q)
	}
}

// enters reports whether the caller is admitted to the app: an actor type
// or a role from app(…) (RFC §22.6).
func enters(sc *reasonr.Schema, app *reasonr.UIApp, c reasonr.Caller) bool {
	if c.Actor != 0 {
		if ti := sc.Type(c.Actor); ti != nil {
			for _, name := range app.Actors {
				if name == ti.Name {
					return true
				}
			}
		}
	}
	for _, r := range c.Roles {
		for _, name := range app.Roles {
			if r == name {
				return true
			}
		}
	}
	return false
}

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

// lang is the response language: the lang parameter, then Accept-Language
// among the languages of the model texts, otherwise the app's base language.
func lang(r *http.Request, ui *reasonr.UI, app *reasonr.UIApp) string {
	if l := r.URL.Query().Get("lang"); l != "" {
		return l
	}
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		tag, _, _ := strings.Cut(strings.TrimSpace(part), ";")
		base, _, _ := strings.Cut(strings.ToLower(tag), "-")
		if base == app.Language {
			return base
		}
		if _, ok := ui.Texts[base]; ok {
			return base
		}
	}
	return app.Language
}

// manifest is GET /app/{app}: the menu, screens with description hashes, places.
func (s *Server) manifest(w http.ResponseWriter, r *http.Request, q *req) {
	m := q.model.Manifest(q.app.Name, lang(r, q.model.UI(), q.app), platform(r))
	m.Test = s.opts.Test
	for i, f := range q.sc.Features {
		if q.caller.Features.Has(i) {
			m.Features = append(m.Features, f.Name)
		}
	}
	// The if conditions of the app layout: the visibility of menu nodes for the caller.
	l := &loader{s: s, q: q, ctx: r.Context(), data: map[int]typed{}, out: map[string]*State{}}
	l.appNodes(q.app.Nodes)
	if l.err != nil {
		s.fail(w, r, l.err)
		return
	}
	if len(l.out) > 0 {
		m.States = map[string]any{}
		for k, st := range l.out {
			m.States[k] = st
		}
	}
	data, err := json.Marshal(m)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, data)
}

func platform(r *http.Request) string {
	if p := r.URL.Query().Get("platform"); p != "" {
		return p
	}
	return "web"
}

// screen is GET /app/{app}/screens/{Screen}.{hash}.json: a screen
// description. The address with the hash never changes, so the description
// is cached forever; an outdated hash is 404: the runtime rereads the manifest.
func (s *Server) screen(w http.ResponseWriter, r *http.Request, q *req) {
	// <screen>.<hash>.json; the screen name is full, with dots (Ordering.MyOrder).
	file, ok := strings.CutSuffix(r.PathValue("file"), ".json")
	i := strings.LastIndex(file, ".")
	name, hash := file, ""
	if i >= 0 {
		name, hash = file[:i], file[i+1:]
	}
	v := s.view(q, name, lang(r, q.model.UI(), q.app), platform(r))
	switch {
	case v == nil:
		http.NotFound(w, r)
	case !ok || hash != v.hash:
		http.Error(w, "the screen description changed: reread the manifest", http.StatusNotFound)
	default:
		w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
		writeJSON(w, http.StatusOK, v.data)
	}
}

type view struct {
	data []byte
	hash string
}

// view returns a screen description in JSON and its hash; nil — no screen.
func (s *Server) view(q *req, name, l, plat string) *view {
	key := viewKey{q.model.UI(), q.app.Name, name + "/" + plat, l}
	s.mu.Lock()
	v, ok := s.views[key]
	s.mu.Unlock()
	if ok {
		return v
	}
	d := q.model.Screen(q.app.Name, name, l, plat)
	if d == nil {
		return nil
	}
	data, err := json.Marshal(d)
	if err != nil {
		return nil
	}
	v = &view{data: data, hash: reasonr.ViewHash(d)}
	s.mu.Lock()
	s.views[key] = v
	s.mu.Unlock()
	return v
}

func writeJSON(w http.ResponseWriter, status int, data []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, msg, http.StatusUnauthorized)
}

// eval evaluates a layout expression with a model function.
func (s *Server) eval(ctx context.Context, q *req, name string, args []reasonr.Value) (reasonr.Value, error) {
	return q.model.Eval(ctx, name, args, q.a, q.caller, s.opts.Now())
}
