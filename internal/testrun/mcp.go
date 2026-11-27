package testrun

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	reasonr "github.com/tycheg/reasonr-compiler"
)

// NewMCPDriver is the mcp driver (RFC §31.3.2): like api, but model
// commands and queries are the server's MCP tools (tools/call in POST /mcp)
// from the step's caller; the tenant, fakes, clock and checks go through
// the test API.
func NewMCPDriver(ctx context.Context, base, model string, s *reasonr.Schema) (*HTTPDriver, error) {
	d, err := NewHTTPDriver(ctx, base, model, s)
	if err != nil {
		return nil, err
	}
	d.mcp = true
	return d, nil
}

// tool turns an /api call into tools/call and the tool response into an
// API response: a query result, an accepted command or business errors (422).
func (d *HTTPDriver) tool(ctx context.Context, method, path, auth string, body []byte) (int, []byte, error) {
	if d.session == "" {
		init := []byte(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"reasonrd test"}}}`)
		if status, data, err := d.send(ctx, "POST", "/mcp", auth, init, nil); err != nil || status != http.StatusOK {
			return 0, nil, fmt.Errorf("mcp initialize: %d %s %v", status, data, err)
		}
	}
	u, err := url.Parse(path)
	if err != nil {
		return 0, nil, err
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/api/"), "/")
	args := map[string]json.RawMessage{}
	var name string
	switch {
	case method == "POST" && len(parts) == 3:
		name = parts[0] + "." + parts[2]
		id, _ := json.Marshal(parts[1])
		args["id"] = id
		if len(body) > 0 {
			if err := json.Unmarshal(body, &args); err != nil {
				return 0, nil, err
			}
		}
	case method == "POST" && len(parts) == 2:
		name = parts[0] + "." + parts[1]
		if len(body) > 0 {
			if err := json.Unmarshal(body, &args); err != nil {
				return 0, nil, err
			}
		}
	case method == "GET" && len(parts) == 2:
		name = parts[0] + "." + parts[1]
		for k, vs := range u.Query() {
			if k == "after" {
				continue
			}
			v, _ := json.Marshal(vs[0]) // the server passes the value to /api as the same text
			args[k] = v
		}
	default:
		return 0, nil, fmt.Errorf("mcp: path %s is not a model call", path)
	}
	d.calls++
	params, _ := json.Marshal(map[string]any{"name": name, "arguments": args})
	req := []byte(`{"jsonrpc":"2.0","id":` + strconv.Itoa(d.calls) + `,"method":"tools/call","params":` + string(params) + `}`)
	status, data, err := d.send(ctx, "POST", "/mcp", auth, req, http.Header{"Mcp-Session-Id": {d.session}})
	if err != nil || status != http.StatusOK {
		return status, data, err
	}
	var resp struct {
		Result *struct {
			IsError           bool            `json:"isError"`
			StructuredContent json.RawMessage `json:"structuredContent"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, nil, err
	}
	switch {
	case resp.Error != nil:
		return http.StatusBadGateway, []byte(resp.Error.Message), nil
	case resp.Result.IsError:
		return http.StatusUnprocessableEntity, resp.Result.StructuredContent, nil
	case method == "GET":
		var q struct {
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(resp.Result.StructuredContent, &q); err != nil {
			return 0, nil, err
		}
		return http.StatusOK, q.Result, nil
	}
	return http.StatusOK, resp.Result.StructuredContent, nil
}
