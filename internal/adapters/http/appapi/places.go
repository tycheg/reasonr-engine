package appapi

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/http/httpapi"
	"github.com/tycheg/reasonr-engine/kind"
	"github.com/tycheg/reasonr-engine/port"
)

// Places (RFC §22): the app API calls only the method of a node of its
// screen. The arguments come from the client whole — fixed values are user
// input too: the method checks access and rules (RFC §22.4).

// placeNode finds the screen and the node: by node number or key.
func placeNode(r *http.Request, q *req) (*reasonr.UIScreen, *reasonr.UINode) {
	sc := q.model.UI().Screen(r.PathValue("screen"))
	if sc == nil || !contains(q.app.Screens, sc.Name) {
		return nil, nil
	}
	ref := r.PathValue("node")
	if id, err := strconv.Atoi(ref); err == nil {
		return sc, sc.Node(id)
	}
	return sc, byKey(sc.Nodes, ref)
}

func byKey(nodes []reasonr.UINode, key string) *reasonr.UINode {
	for i := range nodes {
		if nodes[i].Key == key && nodes[i].Bind != nil {
			return &nodes[i]
		}
		if n := byKey(nodes[i].Children, key); n != nil {
			return n
		}
	}
	return nil
}

// placeQuery is GET /app/{app}/{screen}/{node}?args={…}[&param=name][&after=…]:
// a place query — the next page, filters — or a primitive's query
// parameter (search of EntityPicker).
func (s *Server) placeQuery(w http.ResponseWriter, r *http.Request, q *req) {
	_, n := placeNode(r, q)
	if n == nil {
		http.NotFound(w, r)
		return
	}
	bind := n.Bind
	if name := r.URL.Query().Get("param"); name != "" {
		bind = nil
		for _, pv := range n.Params {
			if pv.Name == name && pv.Query != nil {
				bind = pv.Query
			}
		}
	}
	if bind == nil || bind.Command {
		http.NotFound(w, r)
		return
	}
	ti, mi := methodOf(q.sc, bind)
	if mi == nil || mi.Result == nil {
		http.NotFound(w, r)
		return
	}
	var after port.Position
	if v := r.URL.Query().Get("after"); v != "" {
		p, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			invalid(w, "after", "type")
			return
		}
		after = port.Position(p)
	}
	body := []byte(r.URL.Query().Get("args"))
	if len(body) == 0 {
		body = []byte("{}")
	}
	args, err := q.codec.DecodeArgs(q.a, mi.Params, body)
	if err != nil {
		invalidArgs(w, err)
		return
	}
	out, err := s.x.Query(r.Context(), &kind.QueryRequest{Tenant: q.tenant, After: after,
		Call: reasonr.QueryCall{Type: ti.Ref, Query: mi.Ref, Args: args, Arena: q.a, Caller: q.caller}})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer out.Release()
	if len(out.Errors) > 0 {
		s.rejected(w, r, q, out.Arena, out.Errors)
		return
	}
	st := &State{}
	if st.Data, err = q.codec.Append(nil, out.Arena, *mi.Result, out.Result); err != nil {
		s.fail(w, r, err)
		return
	}
	if bind == n.Bind {
		l := &loader{s: s, q: q, ctx: r.Context(), data: map[int]typed{}, out: map[string]*State{}}
		l.rows(n, typed{v: q.a.Copy(out.Arena, out.Result), t: *mi.Result}, st)
		if l.err != nil {
			s.fail(w, r, l.err)
			return
		}
	}
	data, err := json.Marshal(st)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, data)
}

// command is the body of a place POST: the target and the command arguments in canonical JSON.
type command struct {
	Target json.RawMessage `json:"target"`
	Args   json.RawMessage `json:"args"`
}

// placeCommand is POST /app/{app}/{screen}/{node}: a place command; the
// response is as in the model API (RFC §31.3).
func (s *Server) placeCommand(w http.ResponseWriter, r *http.Request, q *req) {
	_, n := placeNode(r, q)
	if n == nil || n.Bind == nil || !n.Bind.Command {
		http.NotFound(w, r)
		return
	}
	ti, mi := methodOf(q.sc, n.Bind)
	if mi == nil || ti.ID == reasonr.IDNone {
		http.NotFound(w, r)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.opts.MaxBody))
	if err != nil {
		http.Error(w, "request body: "+err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	var body command
	if err := json.Unmarshal(raw, &body); err != nil {
		invalid(w, "", "type")
		return
	}
	id := reasonr.None // a create command: the engine issues the identifier
	if !mi.Create {
		if id, err = q.codec.Decode(q.a, reasonr.Type{Kind: reasonr.KindID, Ref: ti.Ref}, body.Target); err != nil {
			invalid(w, "target", "type")
			return
		}
	}
	if len(body.Args) == 0 {
		body.Args = []byte("{}")
	}
	args, err := q.codec.DecodeArgs(q.a, mi.Params, body.Args)
	if err != nil {
		invalidArgs(w, err)
		return
	}
	res, err := s.x.Command(r.Context(), &kind.CommandRequest{Tenant: q.tenant, Idempotency: r.Header.Get("Idempotency-Key"),
		Call: reasonr.CommandCall{Entity: ti.Ref, ID: id, Command: mi.Ref, Args: args, Arena: q.a, Caller: q.caller}})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer res.Release()
	if len(res.Errors) > 0 {
		s.rejected(w, r, q, res.Arena, res.Errors)
		return
	}
	b := append([]byte(`{"status":"accepted","position":"`), strconv.FormatUint(uint64(res.Position), 10)...)
	b = append(b, '"')
	if res.ID.Kind == reasonr.KindID {
		b = append(b, `,"id":`...)
		if b, err = q.codec.Append(b, res.Arena, reasonr.Type{Kind: reasonr.KindID, Ref: res.ID.Type}, res.ID); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	b = append(b, `,"notifications":[`...)
	for i, nt := range res.Notifications {
		if i > 0 {
			b = append(b, ',')
		}
		if b, err = httpapi.AppendNotification(b, q.codec, res.Arena, nt); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, append(b, "]}"...))
}

// rejected answers business errors as the model API does.
func (s *Server) rejected(w http.ResponseWriter, r *http.Request, q *req, a *reasonr.Arena, errs []reasonr.BusinessError) {
	b := []byte(`{"status":"rejected","errors":[`)
	for i, be := range errs {
		if i > 0 {
			b = append(b, ',')
		}
		var err error
		if b, err = httpapi.AppendError(b, q.codec, a, be); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	writeJSON(w, httpapi.ErrorStatus(errs), append(b, "]}"...))
}

// invalidArgs answers 400 for an argument decoding error.
func invalidArgs(w http.ResponseWriter, err error) {
	path, rule := httpapi.ErrorPath(err)
	invalid(w, path, rule)
}
