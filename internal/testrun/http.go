package testrun

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/codecs"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/kind"
)

var _ kind.TestAPI = (*HTTPDriver)(nil)

// HTTPDriver is a test driver over the API of a server in test mode
// (reasonrd test --driver api, RFC §30–31): each test runs in its own
// ephemeral tenant; a command is a POST, a query a GET with after, time
// offset is /_test/{tenant}/clock; notifications to the initiator come from
// the response, to other actors and external requests from
// /_test/{tenant}/captured. Values are encoded as canonical JSON by the
// local model schema: it must match the server's.
type HTTPDriver struct {
	base   string
	client *http.Client
	tenant string
	s      *reasonr.Schema
	codec  *codecs.Values
	a      *reasonr.Arena
	after  string // the position of the last command
	notes  int    // notifications read
	reqs   int    // external requests read
	fakes  int    // errors of fake calls read
	// mcp is the mcp driver (RFC §31.3.2): model commands and queries go as
	// MCP tools (tools/call in POST /mcp), the rest as for api.
	mcp     bool
	session string // Mcp-Session-Id
	calls   int    // the JSON-RPC call number
	// demo is a demo scenario in a permanent tenant with demo: true (RFC
	// §27.5): the scenario clock is the offset in the Reasonr-Time-Offset
	// header of each request.
	demo   bool
	offset time.Duration
	// observe is the path of a virtual user of a performance test (RFC
	// §27.6): each /api call is measured, nothing is captured.
	observe func(method string, d time.Duration, status int, err error)
}

// NewPerfDriver is the driver of the when path of a performance test in
// the tenant tenant of the server base (the tenant is already filled by
// given): each command or query call is passed to observe — the method
// "Type.method", the time, the response code (0 — a transport fault).
func NewPerfDriver(base, tenant string, s *reasonr.Schema, client *http.Client,
	observe func(method string, d time.Duration, status int, err error)) *HTTPDriver {
	return &HTTPDriver{base: strings.TrimRight(base, "/"), client: client, s: s,
		codec: codecs.JSONOf(s, codecs.Options{}), a: reasonr.NewArena(), tenant: tenant, observe: observe}
}

var _ reasonr.Driver = (*HTTPDriver)(nil)

// NewHTTPDriver creates an ephemeral tenant with the model model (base or
// an extension) on the server base.
func NewHTTPDriver(ctx context.Context, base, model string, s *reasonr.Schema) (*HTTPDriver, error) {
	d := &HTTPDriver{base: strings.TrimRight(base, "/"), client: &http.Client{Timeout: 30 * time.Second}, s: s,
		codec: codecs.JSONOf(s, codecs.Options{}), a: reasonr.NewArena()}
	var res struct{ Tenant string }
	req := map[string]string{}
	if model != Base {
		req["extension"] = model
	}
	body, _ := json.Marshal(req)
	if err := d.call(ctx, "POST", "/_test/tenants", "", body, http.StatusOK, &res); err != nil {
		return nil, err
	}
	d.tenant = res.Tenant
	return d, nil
}

// NewDemoDriver is the driver of a demo scenario (reasonrd demo load): the
// steps go to the tenant tenant with demo: true of the server base by its
// clock with the scenario offset; the tenant is neither created nor deleted.
func NewDemoDriver(base, tenant string, s *reasonr.Schema) *HTTPDriver {
	return &HTTPDriver{base: strings.TrimRight(base, "/"), client: &http.Client{Timeout: 30 * time.Second}, s: s,
		codec: codecs.JSONOf(s, codecs.Options{}), a: reasonr.NewArena(), tenant: tenant, demo: true}
}

// Close deletes the tenant (nothing for a demo).
func (d *HTTPDriver) Close(ctx context.Context) error {
	if d.demo {
		return nil
	}
	return d.call(ctx, "DELETE", "/_test/tenants/"+d.tenant, "", nil, http.StatusNoContent, nil)
}

// EnableFeature switches on a test feature flag in the test's tenant
// (reasonr.FeatureDriver); in a demo the tenant flags are the rollout, and the
// scenario does not change them.
func (d *HTTPDriver) EnableFeature(ctx context.Context, name string) error {
	if d.demo {
		return nil
	}
	return d.call(ctx, "POST", "/_test/"+d.tenant+"/features/"+url.PathEscape(name), "", nil, http.StatusNoContent, nil)
}

// call runs a request and decodes a response with the code want into out.
func (d *HTTPDriver) call(ctx context.Context, method, path, auth string, body []byte, want int, out any) error {
	status, data, err := d.do(ctx, method, path, auth, body)
	if err != nil {
		return err
	}
	if status != want {
		return fmt.Errorf("http: %s %s: %d %s", method, path, status, bytes.TrimSpace(data))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (d *HTTPDriver) do(ctx context.Context, method, path, auth string, body []byte) (int, []byte, error) {
	if d.mcp && strings.HasPrefix(path, "/api/") {
		return d.tool(ctx, method, path, auth, body)
	}
	if d.observe != nil && strings.HasPrefix(path, "/api/") {
		start := time.Now()
		status, data, err := d.send(ctx, method, path, auth, body, nil)
		d.observe(apiMethod(path), time.Since(start), status, err)
		return status, data, err
	}
	return d.send(ctx, method, path, auth, body, nil)
}

// apiMethod is "Type.method" of the path /api/Type/[id/]method[?…].
func apiMethod(path string) string {
	path, _, _ = strings.Cut(strings.TrimPrefix(path, "/api/"), "?")
	parts := strings.Split(path, "/")
	return parts[0] + "." + parts[len(parts)-1]
}

func (d *HTTPDriver) send(ctx context.Context, method, path, auth string, body []byte, header http.Header) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, d.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	req.Header.Set("Content-Type", "application/json")
	if d.demo && d.offset != 0 {
		req.Header.Set("Reasonr-Time-Offset", ports.FormatOffset(d.offset))
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if d.mcp && path == "/mcp" && d.session == "" {
		d.session = resp.Header.Get("Mcp-Session-Id")
	}
	return resp.StatusCode, data, err
}

// auth is the test authentication of the caller in the test's tenant.
func (d *HTTPDriver) auth(src *reasonr.Arena, c reasonr.Caller) (string, error) {
	h := "Test tenant=" + d.tenant
	if c.Subject.Kind == reasonr.KindID {
		id, err := d.idText(src, c.Subject)
		if err != nil {
			return "", err
		}
		h += " actor=" + d.s.Type(c.Subject.Type).Name + ":" + id
	}
	for _, r := range c.Roles {
		h += " role=" + r
	}
	return h, nil
}

// idText is the text form of an identifier (as in an API path).
func (d *HTTPDriver) idText(a *reasonr.Arena, id reasonr.Value) (string, error) {
	b, err := d.codec.Append(nil, a, reasonr.Type{Kind: reasonr.KindID, Ref: id.Type}, id)
	if err != nil {
		return "", err
	}
	return unquote(b), nil
}

// unquote removes the quotes of a JSON string; not a string — as is.
func unquote(b []byte) string {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if json.Unmarshal(b, &s) == nil {
			return s
		}
	}
	return string(b)
}

func (d *HTTPDriver) step() *reasonr.Step {
	d.a.Reset()
	return &reasonr.Step{Arena: d.a, Result: reasonr.None}
}

// rejected is a response with business errors.
type wire struct {
	Status        string            `json:"status"`
	Position      string            `json:"position"`
	Notifications []json.RawMessage `json:"notifications"`
	Errors        []json.RawMessage `json:"errors"`
}

// Command runs a command (reasonr.Driver); a fake command — through the test
// API.
func (d *HTTPDriver) Command(ctx context.Context, call reasonr.CommandCall) (*reasonr.Step, error) {
	ti := d.s.Type(call.Entity)
	mi := &ti.Methods[call.Command]
	if ti.Kind == reasonr.TypeExternal {
		return d.fake(ctx, call, ti, mi)
	}
	auth, err := d.auth(call.Arena, call.Caller)
	if err != nil {
		return nil, err
	}
	id, err := d.idText(call.Arena, call.ID)
	if err != nil {
		return nil, err
	}
	body, err := d.args(call.Arena, mi.Params, call.Args)
	if err != nil {
		return nil, err
	}
	status, data, err := d.do(ctx, "POST", "/api/"+ti.Name+"/"+url.PathEscape(id)+"/"+mi.Name, auth, body)
	if err != nil {
		return nil, err
	}
	st := d.step()
	if err := d.response(st, status, data, call.Caller, call.Arena); err != nil {
		return nil, fmt.Errorf("%s.%s: %w", ti.Name, mi.Name, err)
	}
	return st, d.captured(ctx, st)
}

// fake runs a fake command: POST /_test/{tenant}/fakes/{System}/{Command}.
func (d *HTTPDriver) fake(ctx context.Context, call reasonr.CommandCall, ti *reasonr.TypeInfo, mi *reasonr.MethodInfo) (*reasonr.Step, error) {
	if d.demo {
		return nil, fmt.Errorf("demo scenario: fake command %s.%s — a demo tenant has its external systems on the fake transport (integrations), and the scenario calls the systems' answers itself: [x].Answer(…) as role", ti.Name, mi.Name)
	}
	body, err := d.args(call.Arena, mi.Params, call.Args)
	if err != nil {
		return nil, err
	}
	var res struct{ Errors []json.RawMessage }
	if err := d.call(ctx, "POST", "/_test/"+d.tenant+"/fakes/"+ti.Name+"/"+mi.Name, "", body, http.StatusOK, &res); err != nil {
		return nil, err
	}
	st := d.step()
	for _, raw := range res.Errors {
		be, err := d.decodeError(raw)
		if err != nil {
			return nil, err
		}
		st.Errors = append(st.Errors, be)
	}
	return st, d.captured(ctx, st)
}

// response parses an API response: 200 — notifications to the initiator,
// 400, 403, 422 — business errors, anything else — a fault.
func (d *HTTPDriver) response(st *reasonr.Step, status int, data []byte, caller reasonr.Caller, src *reasonr.Arena) error {
	var w wire
	if status != http.StatusOK && status != http.StatusBadRequest && status != http.StatusForbidden && status != http.StatusUnprocessableEntity ||
		json.Unmarshal(data, &w) != nil {
		return fmt.Errorf("http %d: %s", status, bytes.TrimSpace(data))
	}
	if w.Position != "" {
		d.after = w.Position
	}
	for _, raw := range w.Errors {
		be, err := d.decodeError(raw)
		if err != nil {
			return err
		}
		st.Errors = append(st.Errors, be)
	}
	for _, raw := range w.Notifications {
		n, err := d.decodeNotification(raw)
		if err != nil {
			return err
		}
		n.To = d.a.Copy(src, caller.Subject)
		st.Notifications = append(st.Notifications, n)
	}
	return nil
}

// args encodes the arguments: the passed ones (not none) as a JSON object.
func (d *HTTPDriver) args(a *reasonr.Arena, params []reasonr.FieldInfo, args reasonr.Record) ([]byte, error) {
	b := []byte{'{'}
	vals := a.Fields(args)
	n := 0
	for i := range params {
		if i >= len(vals) || vals[i].Kind == reasonr.KindNone {
			continue
		}
		if n > 0 {
			b = append(b, ',')
		}
		n++
		b = append(reasonr.AppendJSONString(b, params[i].Name), ':')
		t := params[i].Type
		t.Optional = true
		var err error
		if b, err = d.codec.Append(b, a, t, vals[i]); err != nil {
			return nil, fmt.Errorf("argument %s: %w", params[i].Name, err)
		}
	}
	return append(b, '}'), nil
}

type wireMessage struct {
	Name string          `json:"name"`
	Type string          `json:"type"`
	Args json.RawMessage `json:"args"`
	To   *struct {
		Type string          `json:"type"`
		ID   json.RawMessage `json:"id"`
	} `json:"to"`
}

func (d *HTTPDriver) decodeError(raw json.RawMessage) (reasonr.BusinessError, error) {
	var m wireMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return reasonr.BusinessError{}, err
	}
	be := reasonr.BusinessError{Name: m.Name, Args: d.a.Record(0)}
	if m.Type != "" {
		ti := d.s.Lookup(m.Type)
		if ti == nil {
			return be, fmt.Errorf("no type %s in the local model", m.Type)
		}
		be.Owner = ti.Ref
		if mi := ti.Method(m.Name); mi != nil && len(mi.Params) > 0 {
			args, err := d.codec.DecodeFields(d.a, 0, mi.Params, m.Args)
			if err != nil {
				return be, err
			}
			be.Args = args
		}
		return be, nil
	}
	if m.Name == "invalidArgument" {
		var a struct{ Path, Rule string }
		_ = json.Unmarshal(m.Args, &a)
		be.Args = d.a.Record(0, d.a.String(a.Path), d.a.String(a.Rule))
	}
	return be, nil
}

func (d *HTTPDriver) decodeNotification(raw json.RawMessage) (reasonr.Notification, error) {
	var m wireMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return reasonr.Notification{}, err
	}
	ti := d.s.Lookup(m.Type)
	if ti == nil {
		return reasonr.Notification{}, fmt.Errorf("no type %s in the local model", m.Type)
	}
	mi := ti.Method(m.Name)
	if mi == nil {
		return reasonr.Notification{}, fmt.Errorf("no notification %s.%s in the local model", m.Type, m.Name)
	}
	args, err := d.codec.DecodeFields(d.a, 0, mi.Params, m.Args)
	if err != nil {
		return reasonr.Notification{}, err
	}
	n := reasonr.Notification{Owner: ti.Ref, Name: mi.Ref, Args: args, To: reasonr.None}
	if m.To != nil {
		tt := d.s.Lookup(m.To.Type)
		if tt == nil {
			return n, fmt.Errorf("no type %s in the local model", m.To.Type)
		}
		if n.To, err = d.codec.Decode(d.a, reasonr.Type{Kind: reasonr.KindID, Ref: tt.Ref}, m.To.ID); err != nil {
			return n, err
		}
	}
	return n, nil
}

// captured adds to the step the notifications to other actors and the
// external requests the server recorded since the last step.
func (d *HTTPDriver) captured(ctx context.Context, st *reasonr.Step) error {
	if d.demo || d.observe != nil { // demos and load check nothing: nothing is captured
		return nil
	}
	var res struct {
		Notifications []json.RawMessage
		Requests      []struct {
			System, Request string
			Args            json.RawMessage
		}
		FakeErrors []struct {
			System, Entity, Command string
			ID, Args                json.RawMessage
			Errors                  []json.RawMessage
		}
	}
	path := fmt.Sprintf("/_test/%s/captured?notifications=%d&requests=%d&fakeErrors=%d&after=%s", d.tenant, d.notes, d.reqs, d.fakes, d.after)
	if err := d.call(ctx, "GET", path, "", nil, http.StatusOK, &res); err != nil {
		return err
	}
	d.notes += len(res.Notifications)
	d.reqs += len(res.Requests)
	d.fakes += len(res.FakeErrors)
	for _, fe := range res.FakeErrors {
		sys, ti := d.s.Lookup(fe.System), d.s.Lookup(fe.Entity)
		if sys == nil || ti == nil {
			return fmt.Errorf("no types %s, %s in the local model", fe.System, fe.Entity)
		}
		mi := ti.Method(fe.Command)
		if mi == nil {
			return fmt.Errorf("no command %s.%s in the local model", fe.Entity, fe.Command)
		}
		id, err := d.codec.Decode(d.a, reasonr.Type{Kind: reasonr.KindID, Ref: ti.Ref}, fe.ID)
		if err != nil {
			return err
		}
		args, err := d.codec.DecodeFields(d.a, 0, mi.Params, fe.Args)
		if err != nil {
			return err
		}
		out := reasonr.FakeError{Call: reasonr.FakeCall{System: sys.Ref, Entity: ti.Ref, ID: id, Command: mi.Ref, Args: args}}
		for _, raw := range fe.Errors {
			be, err := d.decodeError(raw)
			if err != nil {
				return err
			}
			out.Errors = append(out.Errors, be)
		}
		st.FakeErrors = append(st.FakeErrors, out)
	}
	for _, raw := range res.Notifications {
		n, err := d.decodeNotification(raw)
		if err != nil {
			return err
		}
		st.Notifications = append(st.Notifications, n)
	}
	for _, r := range res.Requests {
		ti := d.s.Lookup(r.System)
		if ti == nil {
			return fmt.Errorf("no external system %s in the local model", r.System)
		}
		mi := ti.Method(r.Request)
		if mi == nil {
			return fmt.Errorf("no request %s.%s in the local model", r.System, r.Request)
		}
		args, err := d.codec.DecodeFields(d.a, 0, mi.Params, r.Args)
		if err != nil {
			return err
		}
		st.Requests = append(st.Requests, reasonr.ExternalRequest{System: ti.Ref, Request: mi.Ref, Args: args, Key: reasonr.None})
	}
	return nil
}

// Query runs a query with the after of the last command (reasonr.Driver).
func (d *HTTPDriver) Query(ctx context.Context, call reasonr.QueryCall) (*reasonr.Step, error) {
	ti := d.s.Type(call.Type)
	mi := &ti.Methods[call.Query]
	auth, err := d.auth(call.Arena, call.Caller)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	if d.after != "" {
		q.Set("after", d.after)
	}
	vals := call.Arena.Fields(call.Args)
	for i := range mi.Params {
		if i >= len(vals) || vals[i].Kind == reasonr.KindNone {
			continue
		}
		p := &mi.Params[i]
		if p.Type.Kind == reasonr.KindPageRequest {
			pr := call.Arena.Elems(vals[i])
			q.Set("size", strconv.FormatInt(pr[0].Int(), 10))
			if pr[1].Kind == reasonr.KindString {
				q.Set("cursor", call.Arena.Text(pr[1]))
			}
			continue
		}
		t := p.Type
		t.Optional = true
		b, err := d.codec.Append(nil, call.Arena, t, vals[i])
		if err != nil {
			return nil, fmt.Errorf("argument %s: %w", p.Name, err)
		}
		q.Set(p.Name, unquote(b))
	}
	status, data, err := d.do(ctx, "GET", "/api/"+ti.Name+"/"+mi.Name+"?"+q.Encode(), auth, nil)
	if err != nil {
		return nil, err
	}
	st := d.step()
	if status == http.StatusOK {
		if st.Result, err = d.codec.Decode(d.a, *mi.Result, data); err != nil {
			return nil, fmt.Errorf("%s.%s: result: %w", ti.Name, mi.Name, err)
		}
		return st, nil
	}
	if err := d.response(st, status, data, call.Caller, call.Arena); err != nil {
		return nil, fmt.Errorf("%s.%s: %w", ti.Name, mi.Name, err)
	}
	return st, nil
}

// Advance moves the tenant clock; timers fire on the server (reasonr.Driver).
func (d *HTTPDriver) Advance(ctx context.Context, dur time.Duration) (*reasonr.Step, error) {
	if d.demo && dur < 0 { // back: only the scenario clock
		d.offset += dur
		return d.step(), nil
	}
	defer func() {
		if d.demo {
			d.offset += dur
		}
	}()
	var res struct{ Errors []json.RawMessage }
	body, _ := json.Marshal(map[string]string{"advance": dur.String()})
	if err := d.call(ctx, "POST", "/_test/"+d.tenant+"/clock", "", body, http.StatusOK, &res); err != nil {
		return nil, err
	}
	st := d.step()
	for _, raw := range res.Errors {
		be, err := d.decodeError(raw)
		if err != nil {
			return nil, err
		}
		st.Errors = append(st.Errors, be)
	}
	return st, d.captured(ctx, st)
}

// NewID issues an identifier: uuid — the driver itself, sequence — the server (reasonr.Driver).
func (d *HTTPDriver) NewID(ctx context.Context, a *reasonr.Arena, t reasonr.TypeRef) (reasonr.Value, error) {
	if t == 0 || d.s.Type(t).ID == reasonr.IDUUID {
		var u [16]byte
		_, _ = rand.Read(u[:])
		u[6] = u[6]&0x0f | 0x40
		u[8] = u[8]&0x3f | 0x80
		if t == 0 {
			return reasonr.TimerID(u), nil
		}
		return reasonr.ID(t, reasonr.UUID(u)), nil
	}
	var res struct{ ID json.RawMessage }
	if err := d.call(ctx, "POST", "/_test/"+d.tenant+"/ids/"+d.s.Type(t).Name, "", nil, http.StatusOK, &res); err != nil {
		return reasonr.None, err
	}
	return d.codec.Decode(a, reasonr.Type{Kind: reasonr.KindID, Ref: t}, res.ID)
}
