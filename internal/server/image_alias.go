package server

import (
	"net/http"
	"net/url"
	"strings"

	"fujipic/internal/config"
)

// Dedicated primary and alias image hosts are image-only. Enforce before auth so
// requests to a custom hostname never refresh a website session or set cookies.
// The reverse proxy must also match the hostname and validate origin TLS/SNI.
func (s *Server) withImageAlias(next http.Handler) http.Handler {
	hosts := make(map[string]bool)
	app, _ := url.Parse(s.cfg.AppBaseURL)
	appHost := ""
	if app != nil {
		appHost = config.NormalizeHostname(app.Host)
	}
	for _, base := range append([]string{s.cfg.PublicBaseURL}, s.cfg.PublicBaseAliases...) {
		if u, err := url.Parse(base); err == nil {
			host := config.NormalizeHostname(u.Host)
			if host != "" && host != appHost {
				hosts[host] = true
			}
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
