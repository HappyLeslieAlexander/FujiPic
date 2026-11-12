package server

import (
	"net/http"
	"net/url"
	"strings"

	"fujipic/internal/config"
)

// Aliases are image-only hosts. Enforce this before session authentication so
// requests to a custom hostname never refresh a website session or set cookies.
// The reverse proxy must also match the hostname and validate origin TLS/SNI.
func (s *Server) withImageAlias(next http.Handler) http.Handler {
	hosts := make(map[string]bool)
	for _, base := range s.cfg.PublicBaseAliases {
		if u, err := url.Parse(base); err == nil {
			hosts[config.NormalizeHostname(u.Host)] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hosts[config.NormalizeHostname(r.Host)] &&
			(!(r.Method == http.MethodGet || r.Method == http.MethodHead) || !strings.HasPrefix(r.URL.Path, "/i/")) {
			w.Header().Set("Cache-Control", "no-store")
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}
