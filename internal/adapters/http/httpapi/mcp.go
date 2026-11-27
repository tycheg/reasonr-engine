package httpapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"

	reasonr "github.com/tycheg/reasonr-compiler"
)

// The model's MCP server (RFC §31.3.2): the interface for AI agents working
// on behalf of a user or a role. The protocol is MCP Streamable HTTP
// (JSON-RPC 2.0 in POST /mcp, the response is JSON). Tools are the public
// commands and queries available to the caller; a call goes through the
// same handlers as /api, with the same authentication and retry key. No
// resources or templates: all reading is query tools.

// MCPProtocol is the server's MCP protocol version.
const MCPProtocol = "2025-06-18"

// mcpKey marks an /api call from an MCP tool (in the request context).
type mcpKey struct{}

func fromMCP(ctx context.Context) bool { return ctx.Value(mcpKey{}) != nil }

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// mcp — POST /mcp.
func (s *Server) mcp(w http.ResponseWriter, r *http.Request, c *call) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.opts.MaxBody))
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" {
		rpcReply(w, nil, nil, &rpcError{Code: -32700, Message: "expected a JSON-RPC 2.0 request"})
		return
	}
	if len(req.ID) == 0 { // a client notification: notifications/initialized and others
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := MCPProtocol
		if p.ProtocolVersion != "" && p.ProtocolVersion < MCPProtocol {
			version = p.ProtocolVersion
		}
		id := make([]byte, 16)
		_, _ = rand.Read(id)
		w.Header().Set("Mcp-Session-Id", hex.EncodeToString(id))
		rpcReply(w, req.ID, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "reasonr", "version": "1"},
			"instructions":    "The tools are the model commands and queries available to you. Commands change data; ask the user before a destructive one.",
		}, nil)
	case "ping":
		rpcReply(w, req.ID, map[string]any{}, nil)
	case "tools/list":
		a := reasonr.GetArena()
		defer reasonr.PutArena(a)
		_, caller, sc, ok := s.authenticate(w, r, c, a, false)
		if !ok {
			return
		}
		rpcReply(w, req.ID, map[string]any{"tools": tools(sc, caller)}, nil)
	case "tools/call":
		s.mcpCall(w, r, req)
	default:
		rpcReply(w, req.ID, nil, &rpcError{Code: -32601, Message: "no method " + req.Method})
	}
}

// tools are the caller's tools: public commands and queries whose access
// the caller can pass (like UI places), by name.
func tools(sc *reasonr.Schema, caller reasonr.Caller) []map[string]any {
	out := []map[string]any{}
	for i := range sc.Types {
		ti := &sc.Types[i]
		if ti.Kind == reasonr.TypeExternal {
			continue
		}
		for j := range ti.Methods {
			mi := &ti.Methods[j]
			switch {
			case !mi.Public, mi.Kind != reasonr.MethodCommand && mi.Kind != reasonr.MethodQuery,
				mi.Kind == reasonr.MethodQuery && mi.Result == nil, !mayCall(sc, mi, caller):
				continue
			}
			out = append(out, reasonr.Tool(sc, ti, mi))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
	return out
}

// mayCall reports whether the caller can pass a method's access: any
// authenticated caller, self and field — the interpreter checks; an actor
// — the caller itself or one acting as it by an ACL; a role — among the
// caller's roles.
func mayCall(sc *reasonr.Schema, mi *reasonr.MethodInfo, c reasonr.Caller) bool {
	for _, acc := range mi.Access {
		switch acc.Kind {
		case reasonr.AccessAny, reasonr.AccessSelf, reasonr.AccessField:
			return true
		case reasonr.AccessActor:
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

// mcpCall is tools/call: a call of this server's /api from the request's caller.
func (s *Server) mcpCall(w http.ResponseWriter, r *http.Request, req rpcRequest) {
	var p struct {
		Name      string                     `json:"name"`
		Arguments map[string]json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		rpcReply(w, req.ID, nil, &rpcError{Code: -32602, Message: "wrong tools/call parameters"})
		return
	}
	i := strings.LastIndex(p.Name, ".")
	if i < 0 {
		rpcReply(w, req.ID, nil, &rpcError{Code: -32602, Message: "no tool " + p.Name})
		return
	}
	typ, name := p.Name[:i], p.Name[i+1:]
	args := p.Arguments
	if args == nil {
		args = map[string]json.RawMessage{}
	}
	var in *http.Request
	query := false
	if id, ok := args["id"]; ok {
		delete(args, "id")
		var text string
		if json.Unmarshal(id, &text) != nil {
			text = string(id)
		}
		body, _ := json.Marshal(args)
		in = httptest.NewRequest(http.MethodPost, "/api/"+url.PathEscape(typ)+"/"+url.PathEscape(text)+"/"+url.PathEscape(name), bytes.NewReader(body))
	} else if s.isCommand(r, typ, name) {
		body, _ := json.Marshal(args)
		in = httptest.NewRequest(http.MethodPost, "/api/"+url.PathEscape(typ)+"/"+url.PathEscape(name), bytes.NewReader(body))
	} else {
		q := url.Values{}
		for k, v := range args {
			var text string
			if json.Unmarshal(v, &text) != nil {
				text = string(v)
			}
			q.Set(k, text)
		}
		query = true
		in = httptest.NewRequest(http.MethodGet, "/api/"+url.PathEscape(typ)+"/"+url.PathEscape(name)+"?"+q.Encode(), nil)
	}
	in = in.WithContext(context.WithValue(r.Context(), mcpKey{}, true))
	for _, h := range []string{"Authorization", VersionHeader, TimeOffsetHeader} {
		if v := r.Header.Get(h); v != "" {
			in.Header.Set(h, v)
		}
	}
	if sid := r.Header.Get("Mcp-Session-Id"); sid != "" { // an agent repeats a call — the same response
		in.Header.Set("Idempotency-Key", "mcp:"+sid+":"+string(req.ID))
	}
	rec := httptest.NewRecorder()
	s.apiHandler().ServeHTTP(rec, in)
	var out map[string]any
	if json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		rpcReply(w, req.ID, nil, &rpcError{Code: -32603, Message: strings.TrimSpace(rec.Body.String())})
		return
	}
	switch {
	case rec.Code == http.StatusOK && query:
		out = map[string]any{"result": json.RawMessage(rec.Body.Bytes())}
	case rec.Code == http.StatusOK, out["status"] == "rejected":
	case rec.Code == http.StatusNotFound:
		rpcReply(w, req.ID, nil, &rpcError{Code: -32602, Message: "no tool " + p.Name})
		return
	default:
		rpcReply(w, req.ID, nil, &rpcError{Code: -32603, Message: strings.TrimSpace(rec.Body.String())})
		return
	}
	text, _ := json.Marshal(out)
	rpcReply(w, req.ID, map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(text)}},
		"structuredContent": out,
		"isError":           rec.Code != http.StatusOK,
	}, nil)
}

// isCommand reports whether the method is a command (a create command has no id).
func (s *Server) isCommand(r *http.Request, typ, name string) bool {
	a := reasonr.GetArena()
	defer reasonr.PutArena(a)
	if s.auth == nil {
		return false
	}
	tenant, _, err := s.auth.Authenticate(r.Context(), credentials(r, r.Header.Get("Authorization")), a)
	if err != nil {
		return false
	}
	sc, err := s.x.Schema(tenant)
	if err != nil {
		return false
	}
	_, mi := sc.Method(typ, name)
	return mi != nil && mi.Kind == reasonr.MethodCommand
}

func rpcReply(w http.ResponseWriter, id json.RawMessage, result any, rerr *rpcError) {
	resp := map[string]any{"jsonrpc": "2.0", "id": id}
	if rerr != nil {
		resp["error"] = rerr
	} else {
		resp["result"] = result
	}
	b, _ := json.Marshal(resp)
	writeJSON(w, http.StatusOK, b)
}

// protectedResource is the protected resource metadata (RFC 9728) for MCP
// clients: where to get a token; the server itself is the resource server.
func (s *Server) protectedResource(w http.ResponseWriter, r *http.Request) {
	scheme := "https"
	if r.TLS == nil {
		scheme = "http"
	}
	b, _ := json.Marshal(map[string]any{
		"resource":                 scheme + "://" + r.Host + "/mcp",
		"authorization_servers":    append([]string{}, s.opts.Issuers...),
		"bearer_methods_supported": []string{"header"},
	})
	writeJSON(w, http.StatusOK, b)
}
