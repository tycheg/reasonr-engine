package httpapi

import (
	"encoding/json"
	"net/http"

	reasonr "github.com/tycheg/reasonr-compiler"
)

// integrationsOpenAPI is GET /openapi/integrations.json: the integration
// service contract of the model of the caller's tenant (as /openapi.json).
func (s *Server) integrationsOpenAPI(w http.ResponseWriter, r *http.Request, c *call) {
	sc, ok := s.openapiSchema(w, r, c)
	if !ok {
		return
	}
	b, err := json.MarshalIndent(IntegrationsOpenAPI(sc), "", "  ")
	if err != nil {
		s.fail(w, r, c, err)
		return
	}
	writeJSON(w, http.StatusOK, append(b, '\n'))
}

// IntegrationsOpenAPI builds the integration service contract (reasonr.IntegrationsOpenAPI).
func IntegrationsOpenAPI(sc *reasonr.Schema) obj { return reasonr.IntegrationsOpenAPI(sc) }
