package appapi

import (
	"io/fs"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strings"
)

// static serves the web runtime build (RFC §22): the ingress.ui.dir
// directory, the built-in build or the ingress.ui.proxy dev server. An
// unknown path gives index.html: the runtime parses the /ui/{App}/{Screen} routes.
func (s *Server) static() http.Handler {
	if s.opts.Proxy != "" {
		u, err := url.Parse(s.opts.Proxy)
		if err != nil {
			return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "ingress.ui.proxy: "+err.Error(), http.StatusBadGateway)
			})
		}
		return httputil.NewSingleHostReverseProxy(u) // and the hot reload WebSocket
	}
	var fsys fs.FS
	switch {
	case s.opts.Dir != "":
		fsys = os.DirFS(s.opts.Dir)
	case s.opts.Bundle != nil:
		fsys = s.opts.Bundle
	default:
		fsys = defaultBundle()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fsys == nil {
			http.Error(w, "no web runtime build is linked into this server: set ingress.ui.dir", http.StatusNotFound)
			return
		}
		name := strings.TrimPrefix(path.Clean(r.URL.Path), "/ui")
		name = strings.TrimPrefix(name, "/")
		if name == "" {
			name = "index.html"
		}
		if st, err := fs.Stat(fsys, name); err != nil || st.IsDir() {
			name = "index.html"
		}
		if strings.HasPrefix(name, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		http.ServeFileFS(w, r, fsys, name)
	})
}
