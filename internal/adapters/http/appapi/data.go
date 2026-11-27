package appapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/http/httpapi"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Screen data (RFC §22): one request per screen — the server runs the node
// queries over the tree, evaluates if conditions and node availability.
// Hidden nodes return no data, and their nested queries do not run. Nodes
// in a table or list row get per-row visibility. A query whose argument
// the user enters and has no default waits for input (pending): a place
// query runs it.

// State is a node state in screen data.
type State struct {
	Data    json.RawMessage   `json:"data,omitempty"`
	Errors  []json.RawMessage `json:"errors,omitempty"`
	Hidden  bool              `json:"hidden,omitempty"`
	Pending bool              `json:"pending,omitempty"`
	// Rows hold the visibility of the row's nodes (by number) in each row of Data.
	Rows map[string][]bool `json:"rows,omitempty"`
}

// Data is the response of GET /app/{app}/{screen}.
type Data struct {
	Position string            `json:"position,omitempty"`
	Nodes    map[string]*State `json:"nodes"`
}

type loader struct {
	s      *Server
	q      *req
	ctx    context.Context
	after  port.Position
	params map[string]reasonr.Value
	data   map[int]typed // the results of node queries in q.a
	out    map[string]*State
	err    error
}

// typed is a value and its schema type.
type typed struct {
	v reasonr.Value
	t reasonr.Type
}

// row is a table or list row of the node node.
type row struct {
	node int
	rec  typed
}

func (s *Server) data(w http.ResponseWriter, r *http.Request, q *req) {
	sc := q.model.UI().Screen(r.PathValue("screen"))
	if sc == nil || !contains(q.app.Screens, sc.Name) {
		http.NotFound(w, r)
		return
	}
	l := &loader{s: s, q: q, ctx: r.Context(), data: map[int]typed{}, out: map[string]*State{}}
	if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			invalid(w, "after", "type")
			return
		}
		l.after = port.Position(n)
	}
	var ok bool
	if l.params, ok = s.screenParams(w, r, q, sc); !ok {
		return
	}
	l.nodes(sc.Nodes)
	if l.err != nil {
		s.fail(w, r, l.err)
		return
	}
	data, err := json.Marshal(Data{Position: positionText(l.after), Nodes: l.out})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, data)
}

func positionText(p port.Position) string {
	if p == 0 {
		return ""
	}
	return strconv.FormatUint(uint64(p), 10)
}

// screenParams decodes the screen parameters from the query string: a value
// is canonical JSON; strings and identifiers may be written without quotes.
func (s *Server) screenParams(w http.ResponseWriter, r *http.Request, q *req, sc *reasonr.UIScreen) (map[string]reasonr.Value, bool) {
	out := map[string]reasonr.Value{}
	for _, p := range sc.Params {
		t, err := typeOf(q.sc, p.Type)
		if err != nil {
			s.fail(w, r, err)
			return nil, false
		}
		raw, given := r.URL.Query()[p.Name]
		if !given {
			if !t.Optional {
				invalid(w, p.Name, "required")
				return nil, false
			}
			out[p.Name] = reasonr.None
			continue
		}
		v, err := q.codec.Decode(q.a, t, jsonText(raw[0]))
		if err != nil {
			invalid(w, p.Name, "type")
			return nil, false
		}
		out[p.Name] = v
	}
	return out, true
}

// jsonText is a parameter value as JSON: a bare string is quoted.
func jsonText(s string) []byte {
	if json.Valid([]byte(s)) {
		return []byte(s)
	}
	b, _ := json.Marshal(s)
	return b
}

func (l *loader) state(id int) *State {
	k := strconv.Itoa(id)
	st := l.out[k]
	if st == nil {
		st = &State{}
		l.out[k] = st
	}
	return st
}

// nodes walks the nodes outside rows.
func (l *loader) nodes(nodes []reasonr.UINode) {
	for i := range nodes {
		if l.err != nil {
			return
		}
		l.node(&nodes[i])
	}
}

func (l *loader) node(n *reasonr.UINode) {
	if n.Bind != nil && !l.allowed(n.Bind) {
		l.state(n.ID).Hidden = true
		return
	}
	if n.If != nil {
		v, ok := l.expr(n.If, nil)
		if ok && v.Kind == reasonr.KindBool && !v.Bool() {
			l.state(n.ID).Hidden = true
			return
		}
	}
	if n.Bind != nil && !n.Bind.Command {
		if !l.query(n) {
			return
		}
	}
	l.nodes(n.Children)
}

// allowed is the caller's access to the binding method (RFC §22.6): by
// roles and actor type. self and field access are checked by the command.
// appNodes computes the visibility of the app layout nodes: binding access
// and the if condition (with a feature flag too). Menu nodes have no data.
func (l *loader) appNodes(nodes []reasonr.UINode) {
	for i := range nodes {
		n := &nodes[i]
		if l.err != nil {
			return
		}
		if n.Bind != nil && !l.allowed(n.Bind) {
			l.state(n.ID).Hidden = true
			continue
		}
		if n.If != nil {
			if v, ok := l.expr(n.If, nil); ok && v.Kind == reasonr.KindBool && !v.Bool() {
				l.state(n.ID).Hidden = true
				continue
			}
		}
		l.appNodes(n.Children)
	}
}

func (l *loader) allowed(b *reasonr.UIBind) bool {
	_, mi := methodOf(l.q.sc, b)
	if mi == nil {
		return false
	}
	return allowed(l.q.sc, mi, l.q.caller)
}

func allowed(sc *reasonr.Schema, mi *reasonr.MethodInfo, c reasonr.Caller) bool {
	for _, acc := range mi.Access {
		switch acc.Kind {
		case reasonr.AccessAny, reasonr.AccessSelf, reasonr.AccessField:
			return true
		case reasonr.AccessActor:
			if c.Actor != 0 && c.Actor == acc.Actor {
				return true
			}
			// An actor of another bounded context that acts as this actor
			// by an ACL (actor Auth.User as Customer, RFC §23.2).
			if ti := sc.Type(acc.Actor); ti != nil && ti.ActsAs(c.Actor) {
				return true
			}
		case reasonr.AccessRole:
			for _, r := range c.Roles {
				if r == acc.Role {
					return true
				}
			}
		}
	}
	return false
}

func methodOf(sc *reasonr.Schema, b *reasonr.UIBind) (*reasonr.TypeInfo, *reasonr.MethodInfo) {
	ti := sc.Lookup(b.Type)
	if ti == nil {
		return nil, nil
	}
	mi := ti.Method(b.Method)
	if mi == nil || !mi.Public {
		return nil, nil
	}
	return ti, mi
}

// query runs a node's query; false — no data (waiting for input or an
// error), the nested nodes are not walked.
func (l *loader) query(n *reasonr.UINode) bool {
	q := l.q
	ti, mi := methodOf(q.sc, n.Bind)
	if mi == nil || mi.Result == nil {
		l.err = errors.New("appapi: no query " + n.Bind.Type + "." + n.Bind.Method)
		return false
	}
	args, ok := l.args(n, mi, nil)
	if !ok {
		l.state(n.ID).Pending = true
		return false
	}
	out, err := l.s.x.Query(l.ctx, &kind.QueryRequest{Tenant: q.tenant, After: l.after,
		Call: reasonr.QueryCall{Type: ti.Ref, Query: mi.Ref, Args: args, Arena: q.a, Caller: q.caller}})
	if err != nil {
		l.err = err
		return false
	}
	defer out.Release()
	st := l.state(n.ID)
	if len(out.Errors) > 0 {
		for _, be := range out.Errors {
			b, err := httpapi.AppendError(nil, q.codec, out.Arena, be)
			if err != nil {
				l.err = err
				return false
			}
			st.Errors = append(st.Errors, b)
		}
		return false
	}
	if st.Data, err = q.codec.Append(nil, out.Arena, *mi.Result, out.Result); err != nil {
		l.err = err
		return false
	}
	res := typed{v: q.a.Copy(out.Arena, out.Result), t: *mi.Result}
	l.data[n.ID] = res
	l.rows(n, res, st)
	return true
}

// rows computes the visibility of a row's nodes for each result row.
func (l *loader) rows(n *reasonr.UINode, res typed, st *State) {
	items, elem, ok := l.items(res)
	if !ok {
		return
	}
	var walk func([]reasonr.UINode)
	walk = func(nodes []reasonr.UINode) {
		for i := range nodes {
			c := &nodes[i]
			if c.If != nil && usesRow(c.If, n.ID) {
				vis := make([]bool, len(items))
				for j, it := range items {
					v, ok := l.expr(c.If, &row{node: n.ID, rec: typed{v: it, t: elem}})
					vis[j] = !ok || v.Kind != reasonr.KindBool || v.Bool()
				}
				if st.Rows == nil {
					st.Rows = map[string][]bool{}
				}
				st.Rows[strconv.Itoa(c.ID)] = vis
			}
			walk(c.Children)
		}
	}
	walk(n.Children)
}

// items are the rows of a page or list and their type.
func (l *loader) items(res typed) ([]reasonr.Value, reasonr.Type, bool) {
	a := l.q.a
	switch res.t.Kind {
	case reasonr.KindPage:
		if e := a.Elems(res.v); len(e) == 2 && res.t.Elem != nil {
			return a.Elems(e[0]), *res.t.Elem, true
		}
	case reasonr.KindList:
		if res.t.Elem != nil {
			return a.Elems(res.v), *res.t.Elem, true
		}
	}
	return nil, reasonr.Type{}, false
}

func usesRow(e *reasonr.UIExpr, node int) bool {
	for _, a := range e.Args {
		if a.From == reasonr.UIFromRow && a.Node == node {
			return true
		}
		if a.Expr != nil && usesRow(a.Expr, node) {
			return true
		}
	}
	return false
}

// args assembles the arguments of a node's query or command: values from
// scopes, defaults of inputs; ok == false — the user enters an argument
// and there is no value.
func (l *loader) args(n *reasonr.UINode, mi *reasonr.MethodInfo, r *row) (reasonr.Record, bool) {
	body := []byte{'{'}
	for _, a := range n.Bind.Args {
		var p *reasonr.FieldInfo
		for i := range mi.Params {
			if mi.Params[i].Name == a.Name {
				p = &mi.Params[i]
			}
		}
		if p == nil {
			continue
		}
		switch {
		case a.From == reasonr.UIFromPrimitive:
			size := 20
			for _, pv := range n.Params {
				if pv.Name == "size" && pv.Value != nil && len(pv.Value.Value) > 0 {
					_ = json.Unmarshal(pv.Value.Value, &size)
				}
			}
			body = l.field(body, a.Name, []byte(`{"size":`+strconv.Itoa(size)+`,"cursor":null}`))
			continue
		case a.From == reasonr.UIFromInput:
			if a.Default || p.Type.Optional {
				continue // a default value or none
			}
			return reasonr.Record{}, false
		}
		v, ok := l.value(a, r)
		if !ok {
			return reasonr.Record{}, false
		}
		b, err := l.q.codec.Append(nil, l.q.a, p.Type, v)
		if err != nil {
			l.err = err
			return reasonr.Record{}, false
		}
		body = l.field(body, a.Name, b)
	}
	body = append(body, '}')
	rec, err := l.q.codec.DecodeArgs(l.q.a, mi.Params, body)
	if err != nil {
		l.err = err
		return reasonr.Record{}, false
	}
	return rec, true
}

func (l *loader) field(body []byte, name string, value []byte) []byte {
	if len(body) > 1 {
		body = append(body, ',')
	}
	nb, _ := json.Marshal(name)
	return append(append(append(body, nb...), ':'), value...)
}

// value is an argument value from a scope; ok == false — the server does
// not have it (a row without a row, a form, an event, an input).
func (l *loader) value(a reasonr.UIArg, r *row) (reasonr.Value, bool) {
	switch a.From {
	case reasonr.UIFromParam:
		v, ok := l.params[a.Field]
		return v, ok
	case reasonr.UIFromMe:
		return l.q.caller.Subject, l.q.caller.Subject.Kind != reasonr.KindNone
	case reasonr.UIFromData:
		if d, ok := l.data[a.Node]; ok {
			return l.recordField(d, a.Field)
		}
	case reasonr.UIFromRow:
		if r != nil && r.node == a.Node {
			return l.recordField(r.rec, a.Field)
		}
	case reasonr.UIFromExpr:
		if a.Expr != nil {
			return l.expr(a.Expr, r)
		}
	}
	return reasonr.None, false
}

// recordField is a value object field by name.
func (l *loader) recordField(d typed, name string) (reasonr.Value, bool) {
	if d.t.Kind != reasonr.KindRecord || d.v.Kind != reasonr.KindRecord {
		return reasonr.None, false
	}
	ti := l.q.sc.Type(d.t.Ref)
	if ti == nil {
		return reasonr.None, false
	}
	elems := l.q.a.Elems(d.v)
	for i, f := range ti.Fields {
		if f.Name == name && i < len(elems) {
			return elems[i], true
		}
	}
	return reasonr.None, false
}

// expr evaluates a layout expression: a constant or a model function over
// scope values.
func (l *loader) expr(e *reasonr.UIExpr, r *row) (reasonr.Value, bool) {
	if e.Func == "" {
		if string(e.Value) == "true" || string(e.Value) == "false" {
			return reasonr.Bool(string(e.Value) == "true"), true
		}
		return reasonr.None, false
	}
	args := make([]reasonr.Value, len(e.Args))
	for i, a := range e.Args {
		v, ok := l.value(a, r)
		if !ok {
			return reasonr.None, false
		}
		args[i] = v
	}
	v, err := l.s.eval(l.ctx, l.q, e.Func, args)
	if err != nil {
		l.err = err
		return reasonr.None, false
	}
	return v, true
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// invalid answers 400 invalidArgument(path, rule).
func invalid(w http.ResponseWriter, path, rule string) {
	body, _ := json.Marshal(map[string]any{"status": "rejected", "errors": []any{
		map[string]any{"name": "invalidArgument", "args": map[string]string{"path": path, "rule": rule}}}})
	writeJSON(w, http.StatusBadRequest, body)
}

// fail answers 500 and logs the reason.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, port.ErrNotReady) {
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusServiceUnavailable, []byte(`{"status":"rejected","errors":[{"name":"notReady"}]}`))
		return
	}
	s.log.Error("appapi", "path", r.URL.Path, "error", err)
	writeJSON(w, http.StatusInternalServerError, []byte(`{"status":"rejected","errors":[{"name":"internalError"}]}`))
}
