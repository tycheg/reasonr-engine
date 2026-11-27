package httpapi

import (
	"net/http"
	"sync"
	"time"

	reasonr "github.com/tycheg/reasonr-compiler"
)

// The external API contract (RFC §31.3.1): outside test mode /api exposes
// only the published methods; a client of an earlier version names it in
// the Reasonr-Version header and sees the API as that version does —
// method, parameter, field and value names from its snapshot
// (reasonr.APIView); a deprecated method answers with Deprecation and
// Sunset headers.

// VersionHeader is the client's API version header: the release date.
const VersionHeader = "Reasonr-Version"

// views hold schemas as versions see them: (schema, date) → view.
type views struct{ m sync.Map }

type viewKey struct {
	s    *reasonr.Schema
	date string
}

// versioned returns the schema as the version from the request header
// sees it; without the header — the schema itself. false — no such
// version: the 400 response is already sent.
func (s *Server) versioned(w http.ResponseWriter, r *http.Request, sc *reasonr.Schema) (*reasonr.Schema, string, bool) {
	date := r.Header.Get(VersionHeader)
	if date == "" {
		return sc, "latest", true
	}
	if v, ok := s.views.m.Load(viewKey{sc, date}); ok {
		return v.(*reasonr.Schema), date, true
	}
	v := sc.API.Version(date)
	if v == nil {
		writeJSON(w, http.StatusBadRequest, []byte(`{"status":"rejected","errors":[{"name":"unknownVersion"}]}`))
		return nil, "", false
	}
	view := reasonr.APIView(sc, v)
	s.views.m.Store(viewKey{sc, date}, view)
	return view, date, true
}

// published checks whether a method is open: in test mode every public
// one; otherwise those published in the contract and the answering
// commands of external systems (the integration service contract). A
// deprecated method gets the Deprecation and Sunset headers (RFC 9745, RFC
// 8594). The call is counted in a metric.
func (s *Server) published(w http.ResponseWriter, r *http.Request, sc *reasonr.Schema, ti *reasonr.TypeInfo, mi *reasonr.MethodInfo, version string) bool {
	full := ti.Name + "." + mi.Name
	mcp := fromMCP(r.Context()) // MCP is a user interface, like the UI: the API contract does not restrict it
	if !s.opts.AllPublic && !mcp && !sc.API.Published(full) && !integrationReply(sc, mi) {
		return false
	}
	if mcp {
		version = "mcp"
	}
	if d := sc.API.Deprecation(full); d != nil {
		w.Header().Set("Deprecation", "true")
		if t, err := time.Parse(time.DateOnly, d.Sunset); err == nil {
			w.Header().Set("Sunset", t.UTC().Format(http.TimeFormat))
		}
	}
	s.apiCalls.Add(1, full, version)
	return true
}

// integrationReply reports that an external system calls the command with
// its role: it is in the integration service contract, not the external API.
func integrationReply(sc *reasonr.Schema, mi *reasonr.MethodInfo) bool {
	for _, acc := range mi.Access {
		if acc.Kind != reasonr.AccessRole {
			continue
		}
		for i := range sc.Types {
			if ti := &sc.Types[i]; ti.Kind == reasonr.TypeExternal && ti.Role == acc.Role {
				return true
			}
		}
	}
	return false
}
