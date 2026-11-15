package server

import (
	"errors"
	"fujipic/internal/auth"
	"net/http"
)

func passwordBusy(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, auth.ErrBusy) {
		return false
	}
	w.Header().Set("Retry-After", "2")
	writeAPIError(w, http.StatusServiceUnavailable, "auth_busy", "请求过于频繁，请稍后再试", nil)
	return true
}
