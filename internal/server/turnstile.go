package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"
)

const turnstileSiteverifyURL = "https://challenges.cloudflare.com/turnstile/v0/siteverify"

// verifyTurnstile gates the existing form handlers. Neither credentials nor
// response bodies/tokens are logged, including on transport and decode errors.
func (s *Server) verifyTurnstile(r *http.Request, action string) bool {
	if !s.cfg.TurnstileEnabled() {
		return true
	}
	tokens := r.PostForm["cf-turnstile-response"]
	if len(tokens) != 1 || len(tokens[0]) == 0 || len(tokens[0]) > 2048 || s.cfg.TurnstileSecret == "" || len(s.cfg.TurnstileHostnames) == 0 {
		return false
	}
	form := url.Values{"secret": {s.cfg.TurnstileSecret}, "response": {tokens[0]}}
	if ip, err := netip.ParseAddr(s.clientIP(r)); err == nil {
		form.Set("remoteip", ip.String())
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, turnstileSiteverifyURL, strings.NewReader(form.Encode()))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.turnstileClient.Do(req)
	if err != nil {
		s.log.Warn("turnstile verification unavailable", "action", action)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		s.log.Warn("turnstile verification HTTP failure", "status", resp.StatusCode)
		return false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16*1024+1))
	var result struct {
		Success  bool   `json:"success"`
		Action   string `json:"action"`
		Hostname string `json:"hostname"`
	}
	if err != nil || len(body) > 16*1024 || json.Unmarshal(body, &result) != nil {
		s.log.Warn("turnstile verification invalid response")
		return false
	}
	return result.Success && result.Action == action && slices.Contains(s.cfg.TurnstileHostnames, result.Hostname)
}
