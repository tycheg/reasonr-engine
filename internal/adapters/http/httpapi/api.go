package httpapi

import (
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/auth"
	"github.com/tycheg/reasonr-engine/internal/codecs"
	"github.com/tycheg/reasonr-engine/internal/ports"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// authenticate determines the tenant and the caller; on an error it answers
// itself and returns ok == false. The token comes from Authorization, for a
// WebSocket and long polling also from the WebSocket subprotocol
// (WSCredentials) or the access_token parameter: a browser sets no WebSocket header.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request, c *call, a *reasonr.Arena, tokenParam bool) (port.Tenant, reasonr.Caller, *reasonr.Schema, bool) {
	h := r.Header.Get("Authorization")
	if h == "" && tokenParam {
		h = WSCredentials(r.Header)
		if t := r.URL.Query().Get("access_token"); h == "" && t != "" {
			h = "Bearer " + t
		}
	}
	if h == "" || s.auth == nil {
		unauthorized(w, "no credentials")
		return "", reasonr.Caller{}, nil, false
	}
	tenant, caller, err := s.auth.Authenticate(r.Context(), credentials(r, h), a)
	if err != nil {
		switch {
		case errors.Is(err, ports.ErrUnauthenticated):
			unauthorized(w, err.Error())
		case errors.Is(err, ports.ErrForbidden):
			http.Error(w, err.Error(), http.StatusForbidden)
		default:
			s.fail(w, r, c, err)
		}
		return "", reasonr.Caller{}, nil, false
	}
	c.tenant = tenant
	c.caller = callerText(a, caller)
	if port.Offset(r.Context()) != 0 && (s.opts.TimeOffset == nil || !s.opts.TimeOffset(tenant)) {
		http.Error(w, TimeOffsetHeader+": time is shifted only in test mode and for tenants with demo: true", http.StatusBadRequest)
		return "", reasonr.Caller{}, nil, false
	}
	sc, err := s.x.Schema(tenant)
	if err != nil {
		if errors.Is(err, port.ErrUnknownTenant) {
			unauthorized(w, err.Error())
		} else {
			s.fail(w, r, c, err)
		}
		return "", reasonr.Caller{}, nil, false
	}
	return tenant, caller, sc, true
}

// WSProtocol is the engine's WebSocket subprotocol: a browser passes the
// credentials next to it (RFC §22): "Sec-WebSocket-Protocol: reasonr,
// bearer.<token>", in test mode "reasonr, test.<base64url of
// "actor=Customer:<id>">".
const WSProtocol = "reasonr"

// WSCredentials returns the Authorization value from the WebSocket
// subprotocols or "".
func WSCredentials(h http.Header) string {
	for _, v := range h.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(v, ",") {
			p = strings.TrimSpace(p)
			if t, ok := strings.CutPrefix(p, "bearer."); ok && t != "" {
				return "Bearer " + t
			}
			if t, ok := strings.CutPrefix(p, "test."); ok {
				if data, err := base64.RawURLEncoding.DecodeString(t); err == nil {
					return "Test " + string(data)
				}
			}
		}
	}
	return ""
}

// credentials are the credentials from the Authorization header h and the
// tenant named by the X-Reasonr-Tenant header (an external system token
// for every tenant, RFC §18).
func credentials(r *http.Request, h string) ports.Credentials {
	cred := auth.ParseAuthorization(h)
	cred.Tenant = port.Tenant(r.Header.Get("X-Reasonr-Tenant"))
	return cred
}

func unauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, msg, http.StatusUnauthorized)
}

func callerText(a *reasonr.Arena, c reasonr.Caller) string {
	var b strings.Builder
	if c.Subject.Kind != reasonr.KindNone {
		b.WriteString(a.Format(c.Subject))
	}
	for _, r := range c.Roles {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString("role=" + r)
	}
	return b.String()
}

// method finds a public method of kind kind of a type by the names in the path.
func method(sc *reasonr.Schema, typeName, name string, kind reasonr.MethodKind) (*reasonr.TypeInfo, *reasonr.MethodInfo) {
	// A public method of an entity — in any of its models (RFC §23).
	ti, mi := sc.Method(typeName, name)
	if mi == nil || mi.Kind != kind || !mi.Public {
		return nil, nil
	}
	return ti, mi
}

// invalid answers 400 invalidArgument(path, rule).
func invalid(w http.ResponseWriter, path, rule string) {
	b := []byte(`{"status":"rejected","errors":[{"name":"invalidArgument","args":{"path":`)
	b = jsonString(b, path)
	b = append(b, `,"rule":`...)
	b = jsonString(b, rule)
	writeJSON(w, http.StatusBadRequest, append(b, "}}]}"...))
}

// invalidFrom answers 400 for a decoding error; prefix is the argument
// path.
func invalidFrom(w http.ResponseWriter, err error, prefix string) {
	path, rule := ErrorPath(err)
	if prefix != "" {
		switch {
		case path == "":
			path = prefix
		case path[0] != '[':
			path = prefix + "." + path
		default:
			path = prefix + path
		}
	}
	invalid(w, path, rule)
}

// ErrorPath gives the path and rule of invalidArgument for an argument
// decoding error.
func ErrorPath(err error) (path, rule string) {
	var je *reasonr.JSONError
	if errors.As(err, &je) {
		return je.Path, je.Rule
	}
	return "", "type"
}

func jsonString(b []byte, s string) []byte { return reasonr.AppendJSONString(b, s) }

// command — POST /api/{Entity}/{id}/{Command}.
func (s *Server) command(w http.ResponseWriter, r *http.Request, c *call) {
	a := reasonr.GetArena()
	defer reasonr.PutArena(a)
	tenant, caller, sc, ok := s.authenticate(w, r, c, a, false)
	if !ok {
		return
	}
	sc, version, ok := s.versioned(w, r, sc)
	if !ok {
		return
	}
	ti, mi := method(sc, r.PathValue("type"), r.PathValue("method"), reasonr.MethodCommand)
	if mi == nil || ti.ID == reasonr.IDNone {
		http.NotFound(w, r)
		return
	}
	if !s.published(w, r, sc, ti, mi, version) {
		http.NotFound(w, r)
		return
	}
	codec := s.codec(sc)
	// A create command (RFC §10.3) is called without an identifier — the
	// engine issues it; with an identifier only in test mode (Test
	// authentication): the test chooses it itself, as with new.
	id := reasonr.None
	testMode := strings.HasPrefix(r.Header.Get("Authorization"), "Test ")
	switch path := r.PathValue("id"); {
	case path == "" && !mi.Create, path != "" && mi.Create && !testMode:
		http.NotFound(w, r)
		return
	case path != "":
		var err error
		if id, err = codec.DecodeIDText(a, ti.Ref, path); err != nil {
			invalidFrom(w, err, "id")
			return
		}
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.opts.MaxBody))
	if err != nil {
		http.Error(w, "request body: "+err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	in, ok := s.requestCodec(w, r)
	if !ok {
		return
	}
	args, err := s.values(sc, in).DecodeArgs(a, mi.Params, body)
	if err != nil {
		invalidFrom(w, err, "")
		return
	}
	res, err := s.x.Command(r.Context(), &kind.CommandRequest{
		Tenant: tenant, Idempotency: r.Header.Get("Idempotency-Key"),
		Call: reasonr.CommandCall{Entity: ti.Ref, ID: id, Command: mi.Ref, Args: args, Arena: a, Caller: caller},
	})
	if err != nil {
		s.failCommand(w, r, c, err)
		return
	}
	defer res.Release()
	c.position = res.Position
	if res.Replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	if out := s.responseCodec(r); out.Name() != codecs.JSON.Name() {
		e := newEnvelope(out, sc, res.Arena)
		status := http.StatusOK
		if len(res.Errors) > 0 {
			status, err = ErrorStatus(res.Errors), e.rejected(res.Errors)
		} else {
			err = e.accepted(res.Position, res.ID, res.Notifications)
		}
		if err != nil {
			s.fail(w, r, c, err)
			return
		}
		writeBody(w, out, status, e.w.Result())
		return
	}
	if len(res.Errors) > 0 {
		s.rejected(w, r, c, sc, res.Arena, res.Errors)
		return
	}
	b := append([]byte(`{"status":"accepted","position":"`), strconv.FormatUint(uint64(res.Position), 10)...)
	b = append(b, '"')
	if res.ID.Kind == reasonr.KindID {
		b = append(b, `,"id":`...)
		if b, err = codec.Append(b, res.Arena, reasonr.Type{Kind: reasonr.KindID, Ref: res.ID.Type}, res.ID); err != nil {
			s.fail(w, r, c, err)
			return
		}
	}
	b = append(b, `,"notifications":`...)
	b, err = s.notifications(b, sc, res.Arena, res.Notifications)
	if err != nil {
		s.fail(w, r, c, err)
		return
	}
	writeJSON(w, http.StatusOK, append(b, '}'))
}

// failCommand answers an executor error.
func (s *Server) failCommand(w http.ResponseWriter, r *http.Request, c *call, err error) {
	switch {
	case errors.Is(err, port.ErrNotReady):
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusServiceUnavailable, []byte(`{"status":"rejected","errors":[{"name":"notReady"}]}`))
	case errors.Is(err, port.ErrUnknownTenant):
		unauthorized(w, err.Error())
	case errors.Is(err, port.ErrMisdirected):
		// Another engine process serves the bounded context (RFC §23.2).
		writeJSON(w, http.StatusMisdirectedRequest, []byte(`{"status":"rejected","errors":[{"name":"misdirected"}]}`))
	default:
		s.fail(w, r, c, err)
	}
}

// rejected answers business errors: forbidden — 403, invalidArgument —
// 400, others — 422.
func (s *Server) rejected(w http.ResponseWriter, r *http.Request, c *call, sc *reasonr.Schema, a *reasonr.Arena, errs []reasonr.BusinessError) {
	status := http.StatusUnprocessableEntity
	for _, be := range errs {
		switch {
		case be.Owner == 0 && be.Name == "forbidden":
			status = http.StatusForbidden
		case be.Owner == 0 && be.Name == "invalidArgument" && status != http.StatusForbidden:
			status = http.StatusBadRequest
		}
	}
	b := []byte(`{"status":"rejected","errors":[`)
	for i, be := range errs {
		if i > 0 {
			b = append(b, ',')
		}
		var err error
		if b, err = s.businessError(b, sc, a, be); err != nil {
			s.fail(w, r, c, err)
			return
		}
	}
	writeJSON(w, status, append(b, "]}"...))
}

// builtinParams are the parameters of built-in errors with arguments (RFC §14).
var builtinParams = map[string][]reasonr.FieldInfo{
	"invalidArgument": {{Name: "path", Type: reasonr.Type{Kind: reasonr.KindString}}, {Ref: 1, Name: "rule", Type: reasonr.Type{Kind: reasonr.KindString}}},
}

// businessError appends an error {"name": …, "args": {…}}.
func (s *Server) businessError(b []byte, sc *reasonr.Schema, a *reasonr.Arena, be reasonr.BusinessError) ([]byte, error) {
	return AppendError(b, s.codec(sc), a, be)
}

// AppendError appends a business error {"name": …, "type": …, "args": {…}}
// in canonical JSON (RFC §31.3).
func AppendError(b []byte, codec *codecs.Values, a *reasonr.Arena, be reasonr.BusinessError) ([]byte, error) {
	sc := codec.Schema()
	b = append(jsonString(append(b, `{"name":`...), be.Name), ',')
	var params []reasonr.FieldInfo
	if ti := sc.Type(be.Owner); ti != nil {
		b = append(jsonString(append(b, `"type":`...), ti.Name), ',')
		if mi := ti.Method(be.Name); mi != nil && mi.Kind == reasonr.MethodError {
			params = mi.Params
		}
	} else {
		params = builtinParams[be.Name]
	}
	if len(params) == 0 || len(a.Fields(be.Args)) != len(params) {
		return append(b[:len(b)-1], '}'), nil
	}
	b = append(b, `"args":`...)
	b, err := codec.AppendFields(b, a, params, be.Args)
	return append(b, '}'), err
}

// ErrorStatus is the response code for business errors: forbidden — 403,
// invalidArgument — 400, others — 422.
func ErrorStatus(errs []reasonr.BusinessError) int {
	status := http.StatusUnprocessableEntity
	for _, be := range errs {
		switch {
		case be.Owner == 0 && be.Name == "forbidden":
			status = http.StatusForbidden
		case be.Owner == 0 && be.Name == "invalidArgument" && status != http.StatusForbidden:
			status = http.StatusBadRequest
		}
	}
	return status
}

// notifications appends an array of notifications {"name": …, "args": {…}}.
func (s *Server) notifications(b []byte, sc *reasonr.Schema, a *reasonr.Arena, ns []reasonr.Notification) ([]byte, error) {
	b = append(b, '[')
	for i, n := range ns {
		if i > 0 {
			b = append(b, ',')
		}
		var err error
		if b, err = s.notification(b, sc, a, n); err != nil {
			return nil, err
		}
	}
	return append(b, ']'), nil
}

func (s *Server) notification(b []byte, sc *reasonr.Schema, a *reasonr.Arena, n reasonr.Notification) ([]byte, error) {
	return AppendNotification(b, s.codec(sc), a, n)
}

// AppendNotification appends a notification {"name": …, "type": …,
// "args": {…}} in canonical JSON.
func AppendNotification(b []byte, codec *codecs.Values, a *reasonr.Arena, n reasonr.Notification) ([]byte, error) {
	sc := codec.Schema()
	ti := sc.Type(n.Owner)
	if ti == nil || int(n.Name) >= len(ti.Methods) {
		return nil, errors.New("httpapi: a notification of an unknown method")
	}
	mi := &ti.Methods[n.Name]
	b = append(jsonString(append(b, `{"name":`...), mi.Name), `,"type":`...)
	b = append(jsonString(b, ti.Name), `,"args":`...)
	b, err := codec.AppendFields(b, a, mi.Params, n.Args)
	return append(b, '}'), err
}

// query is GET /api/{Type}/{Query}?arguments&after=position.
func (s *Server) query(w http.ResponseWriter, r *http.Request, c *call) {
	a := reasonr.GetArena()
	defer reasonr.PutArena(a)
	tenant, caller, sc, ok := s.authenticate(w, r, c, a, false)
	if !ok {
		return
	}
	sc, version, ok := s.versioned(w, r, sc)
	if !ok {
		return
	}
	ti, mi := method(sc, r.PathValue("type"), r.PathValue("method"), reasonr.MethodQuery)
	if mi == nil || mi.Result == nil {
		http.NotFound(w, r)
		return
	}
	if !s.published(w, r, sc, ti, mi, version) {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	var after uint64
	if v := q.Get("after"); v != "" {
		var err error
		if after, err = strconv.ParseUint(v, 10, 64); err != nil {
			invalid(w, "after", "type")
			return
		}
	}
	body, path, rule := queryArgs(mi.Params, q)
	if rule != "" {
		invalid(w, path, rule)
		return
	}
	codec := s.codec(sc)
	args, err := codec.DecodeArgs(a, mi.Params, body)
	if err != nil {
		invalidFrom(w, err, "")
		return
	}
	out, err := s.x.Query(r.Context(), &kind.QueryRequest{Tenant: tenant, After: port.Position(after),
		Call: reasonr.QueryCall{Type: ti.Ref, Query: mi.Ref, Args: args, Arena: a, Caller: caller}})
	if err != nil {
		s.failCommand(w, r, c, err)
		return
	}
	defer out.Release()
	resp := s.responseCodec(r)
	if len(out.Errors) > 0 {
		if resp.Name() != codecs.JSON.Name() {
			e := newEnvelope(resp, sc, out.Arena)
			if err := e.rejected(out.Errors); err != nil {
				s.fail(w, r, c, err)
				return
			}
			writeBody(w, resp, ErrorStatus(out.Errors), e.w.Result())
			return
		}
		s.rejected(w, r, c, sc, out.Arena, out.Errors)
		return
	}
	b, err := s.values(sc, resp).Append(nil, out.Arena, *mi.Result, out.Result)
	if err != nil {
		s.fail(w, r, c, err)
		return
	}
	writeBody(w, resp, http.StatusOK, b)
}

// queryArgs assembles a JSON object of arguments from the query
// parameters: scalars as is (string kinds in quotes), composite ones as
// JSON in the parameter; PageRequest from the size and cursor parameters. A
// repeated or unknown parameter is a rule violation.
func queryArgs(params []reasonr.FieldInfo, q url.Values) (body []byte, path, rule string) {
	known := map[string]bool{"after": true, "access_token": true}
	b := []byte{'{'}
	n := 0
	add := func(name string, raw []byte) {
		if n > 0 {
			b = append(b, ',')
		}
		n++
		b = append(jsonString(b, name), ':')
		b = append(b, raw...)
	}
	for _, p := range params {
		known[p.Name] = true
		if p.Type.Kind == reasonr.KindPageRequest {
			known["size"], known["cursor"] = true, true
			if len(q["size"]) > 1 || len(q["cursor"]) > 1 {
				return nil, p.Name, "count"
			}
			if q.Get("size") == "" && q.Get("cursor") == "" {
				continue
			}
			pr := []byte(`{"size":`)
			if v := q.Get("size"); v != "" {
				pr = append(pr, v...)
			} else {
				pr = append(pr, "20"...)
			}
			pr = append(pr, `,"cursor":`...)
			if v := q.Get("cursor"); v != "" {
				pr = jsonString(pr, v)
			} else {
				pr = append(pr, "null"...)
			}
			add(p.Name, append(pr, '}'))
			continue
		}
		vs := q[p.Name]
		switch {
		case len(vs) == 0:
			continue
		case len(vs) > 1:
			return nil, p.Name, "count"
		}
		if rawJSON(p.Type) || p.Type.Kind == reasonr.KindID && strings.HasPrefix(vs[0], "{") {
			add(p.Name, []byte(vs[0]))
		} else {
			add(p.Name, jsonString(nil, vs[0]))
		}
	}
	for k := range q {
		if !known[k] {
			return nil, k, "unknown"
		}
	}
	return append(b, '}'), "", ""
}

// rawJSON reports whether a parameter of type t is passed in a query as JSON (reasonr.RawJSON).
func rawJSON(t reasonr.Type) bool { return reasonr.RawJSON(t) }
