package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"

	reasonr "github.com/tycheg/reasonr-compiler"
	"github.com/tycheg/reasonr-engine/port"
)

// openapi is GET /openapi.json: the OpenAPI 3.1 of the model of the
// caller's tenant; without credentials — of the default tenant's model.
func (s *Server) openapi(w http.ResponseWriter, r *http.Request, c *call) {
	sc, ok := s.openapiSchema(w, r, c)
	if !ok {
		return
	}
	sc, version, ok := s.versioned(w, r, sc)
	if !ok {
		return
	}
	doc := reasonr.OpenAPI(sc, !s.opts.AllPublic) // outside test mode — what is published
	if version != "latest" {
		doc["info"].(obj)["version"] = version
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		s.fail(w, r, c, err)
		return
	}
	writeJSON(w, http.StatusOK, append(b, '\n'))
}

// openapiSchema returns the schema of the model of the caller's tenant,
// without credentials — of the default tenant; on an error it answers itself.
func (s *Server) openapiSchema(w http.ResponseWriter, r *http.Request, c *call) (*reasonr.Schema, bool) {
	tenant := port.DefaultTenant
	if h := r.Header.Get("Authorization"); h != "" && s.auth != nil {
		t, _, err := s.auth.Authenticate(r.Context(), credentials(r, h), reasonr.NewArena())
		if err != nil {
			unauthorized(w, err.Error())
			return nil, false
		}
		tenant = t
	}
	c.tenant = tenant
	sc, err := s.x.Schema(tenant)
	if err != nil {
		if errors.Is(err, port.ErrUnknownTenant) {
			http.NotFound(w, r)
		} else {
			s.fail(w, r, c, err)
		}
		return nil, false
	}
	return sc, true
}

// OpenAPI builds the model API description (reasonr.OpenAPI): every public method.
func OpenAPI(sc *reasonr.Schema) obj { return reasonr.OpenAPI(sc, false) }

type obj = map[string]any
