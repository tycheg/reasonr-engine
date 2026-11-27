// Package testapi is the server's test mode (reasonrd --test, RFC §30–31):
// the /_test/* API for reasonrd test --driver api and web. Each test works
// in its own ephemeral data tenant with its own model and virtual clock:
// tests run in parallel in one database without cleanup.
//
//	POST   /_test/tenants                    {"extension": "acme", "now": "2026-01-01T00:00:00Z"} → {"tenant": "…"} — without extension — the base model, without now — the current time
//	DELETE /_test/tenants/{tenant}
//	POST   /_test/{tenant}/clock             {"advance": "90s"} → {"errors": [ … ]} — timers fire synchronously; "-720h" — the clock goes back
//	GET    /_test/{tenant}/captured?notifications=N&requests=M — notifications and external requests after N and M
//	POST   /_test/{tenant}/ids/{Type}        → {"id": …} — the next sequence number
//	POST   /_test/{tenant}/fakes/{System}/{Command} {arguments} → {"errors": [ … ]} — a fake command (RFC §18.1)
//
// After a command, a timer and before a captured response the fake cascade
// runs: the requests to created fakes and their calls of model commands;
// captured returns the errors of these commands in fakeErrors. An ordinary
// tenant accepts a fake command too: the fake transport (Fakes) runs its
// requests and fake calls through the delivery queue and the outbox.
package testapi

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/codecs"
	"github.com/tycheg/reasonr-engine/internal/features"
	"github.com/tycheg/reasonr-engine/internal/kernel"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Clock is the test mode clock: virtual for ephemeral tenants, the system
// clock for the others.
type Clock struct {
	mu  sync.Mutex
	now map[port.Tenant]time.Time
}

var _ port.Clock = (*Clock)(nil)

func NewClock() *Clock { return &Clock{now: map[port.Tenant]time.Time{}} }

func (c *Clock) Now(t port.Tenant) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now, ok := c.now[t]; ok {
		return now
	}
	return time.Now().UTC()
}

func (c *Clock) set(t port.Tenant, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now[t] = now.UTC()
}

func (c *Clock) drop(t port.Tenant) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.now, t)
}

// Hub is a hub that also records the notifications of ephemeral tenants.
type Hub struct {
	kind.NotificationHub
	mu   sync.Mutex
	logs map[port.Tenant]*journal
}

type journal struct {
	a     *reasonr.Arena
	notes []reasonr.Notification
}

// NewHub wraps a hub.
func NewHub(inner kind.NotificationHub) *Hub {
	return &Hub{NotificationHub: inner, logs: map[port.Tenant]*journal{}}
}

// Publish passes notifications on and records them for ephemeral tenants.
func (h *Hub) Publish(ctx context.Context, tenant port.Tenant, a *reasonr.Arena, ns []reasonr.Notification) error {
	h.mu.Lock()
	if j := h.logs[tenant]; j != nil {
		for _, n := range ns {
			j.notes = append(j.notes, reasonr.Notification{Owner: n.Owner, Name: n.Name,
				Args: j.a.Copy(a, n.Args.Value()).Record(), To: j.a.Copy(a, n.To)})
		}
	}
	h.mu.Unlock()
	return h.NotificationHub.Publish(ctx, tenant, a, ns)
}

// PublishForgotten passes forgotten subjects to the inner hub if it
// broadcasts them (ports.ForgetHub).
func (h *Hub) PublishForgotten(ctx context.Context, tenant port.Tenant, subjects []port.SubjectRef) error {
	if fh, ok := h.NotificationHub.(ports.ForgetHub); ok {
		return fh.PublishForgotten(ctx, tenant, subjects)
	}
	return nil
}

// OnForgotten subscribes fn to the forgotten subjects of the inner hub.
func (h *Hub) OnForgotten(fn func(port.Tenant, []port.SubjectRef)) {
	if fh, ok := h.NotificationHub.(ports.ForgetHub); ok {
		fh.OnForgotten(fn)
	}
}

// Close closes the inner hub if it can.
func (h *Hub) Close() error {
	if c, ok := h.NotificationHub.(port.Closer); ok {
		return c.Close()
	}
	return nil
}

// Dropper is a store that deletes a tenant's data.
type Dropper interface {
	DropTenant(ctx context.Context, tenant port.Tenant) error
}

// API is the /_test/* handler. Kernel is set after the kernel is built.
type API struct {
	// Engine is the engine of test API calls; nil — the Kernel with the
	// fields Programs, Timers, Effects, Store and Clock (the stateless engine).
	Engine   Engine
	Kernel   kind.Kernel
	Programs *kernel.Programs
	Models   map[string]reasonr.Interpreter // models by name: base or a tenant's extension
	Timers   kind.TimerSource
	Effects  kind.EffectSource
	Store    kind.Reader // the main store: whether a fake instance exists
	Clock    *Clock
	Hub      *Hub
	Stores   []Dropper
	// Features is the feature flag rollout: test flags switch on in the
	// test's tenant (RFC §23.4).
	Features *features.Resolver
	// Demo holds the tenants with demo: true (RFC §27.5): the scenario
	// clock by the Reasonr-Time-Offset header.
	Demo map[string]bool

	mu      sync.Mutex
	tenants map[port.Tenant]*ephemeral
}

type ephemeral struct {
	mu       sync.Mutex
	read     port.Position // outbox read
	cascaded port.Position // outbox processed by the fake cascade
	// need is the position of the tenant's last known commit: of its
	// commands from the API (the after parameter of captured), fakes and
	// timers. The outbox is read up to it even if it is not visible at once (pending).
	need     port.Position
	a        *reasonr.Arena
	requests []reasonr.ExternalRequest
	fakeErrs []json.RawMessage // errors of commands the fakes called
}

// DemoHandler holds the production server routes for demo tenants (demo:
// true, RFC §27.5): the scenario clock and issuing numbers.
//
//	POST /_test/{tenant}/clock  {"advance": "90s"} with Reasonr-Time-Offset → {"errors": [ … ]}
//	POST /_test/{tenant}/ids/{Type} → {"id": …}
func (api *API) DemoHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /_test/{tenant}/clock", api.clock)
	mux.HandleFunc("POST /_test/{tenant}/ids/{type}", api.id)
	return mux
}

// Handler returns the /_test/* routes.
func (api *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /_test/tenants", api.create)
	mux.HandleFunc("DELETE /_test/tenants/{tenant}", api.remove)
	mux.HandleFunc("POST /_test/{tenant}/clock", api.clock)
	mux.HandleFunc("GET /_test/{tenant}/captured", api.captured)
	mux.HandleFunc("POST /_test/{tenant}/ids/{type}", api.id)
	mux.HandleFunc("POST /_test/{tenant}/fakes/{system}/{command}", api.fake)
	mux.HandleFunc("POST /_test/{tenant}/features/{feature}", api.feature)
	mux.HandleFunc("GET /_test/features", api.features)
	mux.HandleFunc("POST /_test/features", api.changeFeature)
	mux.HandleFunc("GET /_test/features/log", api.featureLog)
	return mux
}

func fail(w http.ResponseWriter, code int, format string, args ...any) {
	http.Error(w, fmt.Sprintf(format, args...), code)
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (api *API) create(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Extension string    `json:"extension"`
		Now       time.Time `json:"now"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "body: %v", err)
		return
	}
	model := cmp.Or(req.Extension, "base")
	m := api.Models[model]
	if m == nil || req.Extension == "base" {
		fail(w, http.StatusNotFound, "no extension %q", req.Extension)
		return
	}
	if req.Now.IsZero() {
		req.Now = time.Now().UTC() // the test clock goes from the current time (RFC §27.5)
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	tenant := port.Tenant("test-" + hex.EncodeToString(b[:]))
	if err := api.engine().CreateTenant(r.Context(), tenant, model, req.Now); err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	api.mu.Lock()
	if api.tenants == nil {
		api.tenants = map[port.Tenant]*ephemeral{}
	}
	api.tenants[tenant] = &ephemeral{a: reasonr.NewArena()}
	api.mu.Unlock()
	api.Hub.mu.Lock()
	api.Hub.logs[tenant] = &journal{a: reasonr.NewArena()}
	api.Hub.mu.Unlock()
	reply(w, map[string]string{"tenant": string(tenant)})
}

func (api *API) tenant(w http.ResponseWriter, r *http.Request) (port.Tenant, *ephemeral, bool) {
	t := port.Tenant(r.PathValue("tenant"))
	api.mu.Lock()
	e := api.tenants[t]
	api.mu.Unlock()
	if e == nil && api.Demo[string(t)] {
		return t, nil, true // a demo tenant: not ephemeral, no notification capture
	}
	if e == nil {
		fail(w, http.StatusNotFound, "no ephemeral tenant %q", t)
		return "", nil, false
	}
	return t, e, true
}

func (api *API) remove(w http.ResponseWriter, r *http.Request) {
	t, _, ok := api.tenant(w, r)
	if !ok {
		return
	}
	for _, d := range api.Stores {
		if err := d.DropTenant(r.Context(), t); err != nil {
			fail(w, http.StatusInternalServerError, "%v", err)
			return
		}
	}
	if err := api.engine().DropTenant(r.Context(), t); err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	api.Hub.mu.Lock()
	delete(api.Hub.logs, t)
	api.Hub.mu.Unlock()
	api.mu.Lock()
	delete(api.tenants, t)
	api.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// feature switches a feature flag on in the test's tenant: a feature line
// at the start of a test. The flag is the full name ("Ordering.tips") or
// the short one if it is unique in the model.
func (api *API) feature(w http.ResponseWriter, r *http.Request) {
	t, _, ok := api.tenant(w, r)
	if !ok {
		return
	}
	s, err := api.Programs.Schema(t)
	if err != nil {
		fail(w, http.StatusNotFound, "%v", err)
		return
	}
	name := features.Resolve(s, r.PathValue("feature"))
	if name == "" {
		fail(w, http.StatusNotFound, "the model has no feature flag %q", r.PathValue("feature"))
		return
	}
	if err := api.engine().SetTestFeature(r.Context(), t, name); err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// FeatureState holds the models' code flags and their rollout: GET
// /_test/features (reasonrd feature list --url, RFC §23.4).
type FeatureState struct {
	Code  []CodeFeature      `json:"code"`
	Rules []port.FeatureRule `json:"rules"`
}

// CodeFeature is a code flag and the models that have it.
type CodeFeature struct {
	reasonr.FeatureInfo
	Models []string `json:"models"`
}

func (api *API) features(w http.ResponseWriter, _ *http.Request) {
	byName := map[string]*CodeFeature{}
	var names []string
	for model, m := range api.Models {
		for _, f := range m.Schema().Features {
			cf := byName[f.Name]
			if cf == nil {
				cf = &CodeFeature{FeatureInfo: f}
				byName[f.Name] = cf
				names = append(names, f.Name)
			}
			cf.Models = append(cf.Models, model)
		}
	}
	sort.Strings(names)
	out := FeatureState{Code: []CodeFeature{}, Rules: api.Features.Rules()}
	for _, n := range names {
		sort.Strings(byName[n].Models)
		out.Code = append(out.Code, *byName[n])
	}
	if out.Rules == nil {
		out.Rules = []port.FeatureRule{}
	}
	reply(w, out)
}

// changeFeature changes the rollout of a test mode engine: POST
// /_test/features with a port.FeatureChange (reasonrd feature on|off --url).
func (api *API) changeFeature(w http.ResponseWriter, r *http.Request) {
	var c port.FeatureChange
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		fail(w, http.StatusBadRequest, "body: %v", err)
		return
	}
	if api.Features == nil || api.Features.Store == nil {
		fail(w, http.StatusNotImplemented, "the engine store does not keep feature flags")
		return
	}
	if err := api.Features.Change(r.Context(), c); err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// featureLog is the rollout change journal: GET /_test/features/log?feature=.
func (api *API) featureLog(w http.ResponseWriter, r *http.Request) {
	if api.Features == nil || api.Features.Store == nil {
		fail(w, http.StatusNotImplemented, "the engine store does not keep feature flags")
		return
	}
	log, err := api.Features.Store.FeatureLog(r.Context(), r.URL.Query().Get("feature"))
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	if log == nil {
		log = []port.FeatureChange{}
	}
	reply(w, log)
}

// clock moves the tenant's clock: while there is a timer with a moment no
// later than the target, the clock moves to its moment and its command
// runs (as time offset in memory); the response is the business errors of
// the timer commands.
func (api *API) clock(w http.ResponseWriter, r *http.Request) {
	t, e, ok := api.tenant(w, r)
	if !ok {
		return
	}
	var req struct {
		Advance string `json:"advance"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "body: %v", err)
		return
	}
	d, err := time.ParseDuration(req.Advance)
	if err != nil {
		fail(w, http.StatusBadRequest, "advance: %q", req.Advance)
		return
	}
	// The scenario clock (RFC §27.5): with an offset in the header or for a
	// demo tenant the tenant clock does not change — timers fire by their
	// scenario moment. Otherwise — the virtual clock of an ephemeral tenant.
	var offset time.Duration
	if v := r.Header.Get("Reasonr-Time-Offset"); v != "" {
		if offset, err = ports.ParseOffset(v); err != nil {
			fail(w, http.StatusBadRequest, "Reasonr-Time-Offset: %v", err)
			return
		}
	}
	scenario := offset != 0 || e == nil
	s, _ := api.Programs.Schema(t)
	codec := codecs.JSONOf(s, codecs.Options{})
	errs := []json.RawMessage{}
	err = api.engine().Advance(r.Context(), t, d, offset, scenario, func(res *kind.CommandResult) error {
		for _, be := range res.Errors {
			b, err := EncodeError(codec, res.Arena, be)
			if err != nil {
				return err
			}
			errs = append(errs, b)
		}
		if e == nil { // a demo tenant: no fake cascade
			return nil
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		e.need = max(e.need, res.Position)
		return api.cascade(r.Context(), t, e)
	})
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	reply(w, map[string]any{"errors": errs})
}

// EncodeError is an error {"name", "type", "args"} as in an API response.
func EncodeError(c *codecs.Values, a *reasonr.Arena, be reasonr.BusinessError) (json.RawMessage, error) {
	out := map[string]json.RawMessage{"name": quote(be.Name)}
	if ti := c.Schema().Type(be.Owner); ti != nil {
		out["type"] = quote(ti.Name)
		if mi := ti.Method(be.Name); mi != nil && mi.Kind == reasonr.MethodError {
			args, err := c.AppendFields(nil, a, mi.Params, be.Args)
			if err != nil {
				return nil, err
			}
			out["args"] = args
		}
	}
	return json.Marshal(out)
}

func quote(s string) json.RawMessage { return reasonr.AppendJSONString(nil, s) }

// pendingWait is how long pending waits for the position need to become visible.
const pendingWait = 10 * time.Second

// pending returns the outbox units of work after cursor. Reading the
// outbox stops at positions whose transactions are still running — other
// tenants' included — so one's own commit with the position need may not
// be visible at once: until cursor reaches need, pending waits for new
// units. An empty result — everything up to need is read.
func (api *API) pending(ctx context.Context, cursor, need port.Position) ([]kind.CommittedUnit, error) {
	deadline := time.Now().Add(pendingWait)
	for {
		units, err := api.engine().Read(ctx, cursor, 0)
		if err != nil || len(units) > 0 || cursor >= need {
			return units, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("outbox: position %d is not visible for longer than %s", need, pendingWait)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// captured gives the tenant's notifications (from the hub) and external
// requests (from the outbox) after the numbers notifications and requests;
// after is the position of the tenant's last command the client has seen:
// the outbox is read up to it.
func (api *API) captured(w http.ResponseWriter, r *http.Request) {
	t, e, ok := api.tenant(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	fromN, _ := strconv.Atoi(q.Get("notifications"))
	fromR, _ := strconv.Atoi(q.Get("requests"))
	fromF, _ := strconv.Atoi(q.Get("fakeErrors"))
	after, _ := strconv.ParseUint(q.Get("after"), 10, 64)
	s, _ := api.Programs.Schema(t)
	codec := codecs.JSONOf(s, codecs.Options{})
	e.mu.Lock()
	defer e.mu.Unlock()
	e.need = max(e.need, port.Position(after))
	if err := api.cascade(r.Context(), t, e); err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	for {
		units, err := api.pending(r.Context(), e.read, e.need)
		if err != nil {
			fail(w, http.StatusInternalServerError, "%v", err)
			return
		}
		if len(units) == 0 {
			break
		}
		for _, u := range units {
			e.read = u.Position
			if u.Tenant != t {
				continue
			}
			for _, rq := range u.Requests {
				rq.Args = e.a.Copy(u.Arena, rq.Args.Value()).Record()
				rq.Key = e.a.Copy(u.Arena, rq.Key)
				e.requests = append(e.requests, rq)
			}
		}
	}
	notes := []json.RawMessage{}
	api.Hub.mu.Lock()
	j := api.Hub.logs[t]
	for i := fromN; j != nil && i < len(j.notes); i++ {
		b, err := EncodeNotification(codec, j.a, j.notes[i])
		if err != nil {
			api.Hub.mu.Unlock()
			fail(w, http.StatusInternalServerError, "%v", err)
			return
		}
		notes = append(notes, b)
	}
	api.Hub.mu.Unlock()
	reqs := []json.RawMessage{}
	for i := fromR; i < len(e.requests); i++ {
		rq := e.requests[i]
		ti := s.Type(rq.System)
		mi := &ti.Methods[rq.Request]
		args, err := codec.AppendFields(nil, e.a, mi.Params, rq.Args)
		if err != nil {
			fail(w, http.StatusInternalServerError, "%v", err)
			return
		}
		b, _ := json.Marshal(map[string]any{"system": ti.Name, "request": mi.Name, "args": json.RawMessage(args)})
		reqs = append(reqs, b)
	}
	fakeErrs := []json.RawMessage{}
	if fromF < len(e.fakeErrs) {
		fakeErrs = e.fakeErrs[fromF:]
	}
	reply(w, map[string]any{"notifications": notes, "requests": reqs, "fakeErrors": fakeErrs})
}

// EncodeNotification is a notification {"name", "type", "args", "to": {"type", "id"}}.
func EncodeNotification(c *codecs.Values, a *reasonr.Arena, n reasonr.Notification) (json.RawMessage, error) {
	ti := c.Schema().Type(n.Owner)
	mi := &ti.Methods[n.Name]
	args, err := c.AppendFields(nil, a, mi.Params, n.Args)
	if err != nil {
		return nil, err
	}
	to, err := c.Append(nil, a, reasonr.Type{Kind: reasonr.KindID, Ref: n.To.Type}, n.To)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"name": mi.Name, "type": ti.Name, "args": json.RawMessage(args),
		"to": map[string]any{"type": c.Schema().Type(n.To.Type).Name, "id": json.RawMessage(to)}})
}

// id issues the next identifier of a type (sequence; uuid works too).
func (api *API) id(w http.ResponseWriter, r *http.Request) {
	t, _, ok := api.tenant(w, r)
	if !ok {
		return
	}
	s, _ := api.Programs.Schema(t)
	ti := s.Lookup(r.PathValue("type"))
	if ti == nil {
		fail(w, http.StatusNotFound, "no type %q", r.PathValue("type"))
		return
	}
	v, err := api.engine().NewID(r.Context(), t, ti.Ref)
	if err != nil {
		fail(w, http.StatusBadRequest, "%v", err)
		return
	}
	b, err := codecs.JSONOf(s, codecs.Options{}).Append(nil, reasonr.NewArena(), reasonr.Type{Kind: reasonr.KindID, Ref: ti.Ref}, v)
	if err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	reply(w, map[string]json.RawMessage{"id": b})
}
