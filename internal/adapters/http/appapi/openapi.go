package appapi

import (
	"encoding/json"
	"net/http"
	"strconv"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/internal/adapters/http/httpapi"
)

type obj = map[string]any

// openapi is GET /app/{app}/openapi.json: the app contract — the manifest,
// screen data and places. The schemas of arguments and results come from
// the model OpenAPI: a place calls the same method.
func (s *Server) openapi(w http.ResponseWriter, _ *http.Request, q *req) {
	data, err := json.MarshalIndent(OpenAPI(q.sc, q.model.UI(), q.app), "", "  ")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, data)
}

// OpenAPI builds the app contract.
func OpenAPI(sc *reasonr.Schema, ui *reasonr.UI, app *reasonr.UIApp) obj {
	model := httpapi.OpenAPI(sc)
	modelPaths, _ := model["paths"].(map[string]any)
	op := func(path, method string) obj {
		if p, ok := modelPaths[path].(map[string]any); ok {
			if o, ok := p[method].(map[string]any); ok {
				return o
			}
		}
		return obj{}
	}
	paths := obj{}
	base := "/app/" + app.Name
	paths[base] = obj{"get": obj{"summary": "The app manifest", "responses": obj{"200": obj{"description": "Manifest"}}}}
	for _, p := range app.Places {
		path := base + "/" + p.Screen + "/" + strconv.Itoa(p.Node)
		if p.Command {
			m := op("/api/"+p.Type+"/{id}/"+p.Method, "post")
			var args any = obj{"type": "object"}
			if rb, ok := m["requestBody"].(map[string]any); ok {
				if c, ok := rb["content"].(map[string]any); ok {
					if j, ok := c["application/json"].(map[string]any); ok {
						args = j["schema"]
					}
				}
			}
			paths[path] = obj{"post": obj{
				"summary":     p.Name,
				"operationId": p.Name,
				"requestBody": obj{"required": true, "content": obj{"application/json": obj{"schema": obj{
					"type": "object", "required": []string{"target"},
					"properties": obj{"target": obj{"description": "The identifier of " + p.Type}, "args": args},
				}}}},
				"responses": m["responses"],
			}}
			continue
		}
		m := op("/api/"+p.Type+"/"+p.Method, "get")
		paths[path] = obj{"get": obj{
			"summary":     p.Name,
			"operationId": p.Name,
			"parameters": []any{
				obj{"name": "args", "in": "query", "description": "Query arguments: an object in canonical JSON", "schema": obj{"type": "string"}},
				obj{"name": "after", "in": "query", "description": "The unit of work position after a command", "schema": obj{"type": "string"}},
			},
			"responses": m["responses"],
		}}
	}
	for _, name := range app.Screens {
		sc := ui.Screen(name)
		var params []any
		for _, p := range sc.Params {
			params = append(params, obj{"name": p.Name, "in": "query", "required": true, "schema": obj{"type": "string"}, "description": p.Type})
		}
		paths[base+"/"+name] = obj{"get": obj{"summary": "Screen data of " + name, "parameters": params,
			"responses": obj{"200": obj{"description": "Screen node data"}}}}
	}
	model["paths"] = paths
	if info, ok := model["info"].(map[string]any); ok {
		info["title"] = "ReasonR app " + app.Name
	}
	return model
}
