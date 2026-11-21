package server

import (
	"errors"
	"net/http"

	"fujipic/internal/auth"
)

func (s *Server) passwordBusy(w http.ResponseWriter, r *http.Request, err error) bool {
	if !errors.Is(err, auth.ErrBusy) {
		return false
	}
	w.Header().Set("Retry-After", "2")
	s.setFlash(w, translate(responseLanguage(w), "请求过于频繁，请稍后再试"))
	target := "/settings"
	switch r.URL.Path {
	case "/login", "/register":
		target = r.URL.Path
	case "/admin/users":
		target = "/admin"
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
	return true
}
