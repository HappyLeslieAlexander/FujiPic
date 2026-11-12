package server

import (
	"errors"
	"net/http"

	"fujipic/internal/store"
)

func (s *Server) handleSessionRevoke(w http.ResponseWriter, r *http.Request) {
	user := s.requireWebUser(w, r)
	if user == nil {
		return
	}
	if !s.validCSRF(r) {
		http.Error(w, "CSRF 校验失败", http.StatusForbidden)
		return
	}
	current := authFrom(r).SessionID
	target := r.PathValue("id")
	var err error
	if target == "" {
		err = s.store.DeleteUserSessionsExcept(r.Context(), user.ID, current)
	} else {
		err = s.store.DeleteOtherUserSession(r.Context(), user.ID, target, current)
	}
	switch {
	case err == nil:
		action := "user.session_revoke"
		message := "该会话已注销。"
		if target == "" {
			action = "user.sessions_revoke_others"
			target = user.ID
			message = "其他会话已全部注销，当前登录已保留。"
		}
		s.audit(r.Context(), user.ID, action, target, s.clientIP(r), "")
		s.setFlash(w, message)
	case errors.Is(err, store.ErrNotFound):
		s.setFlash(w, "会话不存在、已注销或为当前会话。")
	default:
		s.log.Error("revoke session", "err", err)
		s.setFlash(w, "注销会话失败，请稍后重试。")
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}
