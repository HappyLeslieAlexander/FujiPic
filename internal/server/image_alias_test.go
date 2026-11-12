package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"fujipic/internal/config"
)

func TestImageAliasHostSpellings(t *testing.T) {
	s := &Server{cfg: &config.Config{PublicBaseAliases: []string{"https://alt.example.test"}}}
	h := s.withImageAlias(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Set-Cookie", "session=refreshed")
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, host := range []string{"alt.example.test", "ALT.example.test:443", "alt.example.test.", "alt.example.test.:8443"} {
		for _, path := range []string{"/login", "/admin", "/api/v1/me"} {
			r := httptest.NewRequest("GET", "http://localhost"+path, nil)
			r.Host = host
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusNotFound || w.Header().Get("Set-Cookie") != "" {
				t.Errorf("host=%s path=%s exposed website: %d", host, path, w.Code)
			}
		}
	}
}
