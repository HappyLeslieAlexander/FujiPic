package server_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"fujipic/internal/auth"
	"fujipic/internal/id"
	"fujipic/internal/model"
)

func TestSettingsSessionManagement(t *testing.T) {
	env := newTestEnv(t, nil)
	ctx := context.Background()
	env.register(t, env.client, "sessions@example.test", "password123", "")
	other := env.newClient()
	env.login(t, other, "sessions@example.test", "password123")
	third := env.newClient()
	env.login(t, third, "sessions@example.test", "password123")
	outsider := env.newClient()
	env.register(t, outsider, "outsider@example.test", "password123", "")
	sessionID := func(c *http.Client) string {
		t.Helper()
		u, _ := url.Parse(env.ts.URL)
		for _, cookie := range c.Jar.Cookies(u) {
			if cookie.Name == "fujipic_session" {
				_, sid, err := env.store.SessionUser(ctx, id.Hash(cookie.Value))
				if err != nil {
					t.Fatal(err)
				}
				return sid
			}
		}
		t.Fatal("missing session")
		return ""
	}
	currentID, otherID, thirdID, outsiderID := sessionID(env.client), sessionID(other), sessionID(third), sessionID(outsider)
	user, err := env.store.UserByEmail(ctx, "sessions@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.store.CreateSession(ctx, &model.Session{ID: "expired", UserID: user.ID, TokenHash: "expired-hash", ExpiresAt: time.Now().Add(-time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	if _, err := env.db.Exec(`UPDATE sessions SET user_agent=?, ip=? WHERE id=?`, `<script>alert(1)</script>`, "192.0.2.10", otherID); err != nil {
		t.Fatal(err)
	}
	csrf := env.csrf(t, env.client)
	resp, err := env.client.Get(env.ts.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 {
		t.Fatal("settings failed")
	}
	for _, text := range []string{"当前会话", "192.0.2.10", "&lt;script&gt;", "/settings/sessions/" + otherID + "/revoke", "/settings/sessions/" + thirdID + "/revoke"} {
		if !strings.Contains(string(body), text) {
			t.Fatalf("settings missing %q", text)
		}
	}
	for _, text := range []string{"<script>alert", outsiderID, "expired-hash", "/settings/sessions/expired/revoke", "/settings/sessions/" + currentID + "/revoke"} {
		if strings.Contains(string(body), text) {
			t.Fatalf("settings exposed %q", text)
		}
	}
	sessions, err := env.store.ListUserSessions(ctx, user.ID)
	if err != nil || len(sessions) != 3 {
		t.Fatalf("active session list=%v err=%v", sessions, err)
	}
	for _, sess := range sessions {
		if sess.TokenHash != "" || sess.UserID != user.ID {
			t.Fatal("session list exposed authentication hash or another owner")
		}
	}
	post := func(c *http.Client, path, token string, status int) {
		t.Helper()
		r, err := c.PostForm(env.ts.URL+path, url.Values{"csrf": {token}})
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != status {
			t.Fatalf("POST %s status=%d want=%d", path, r.StatusCode, status)
		}
	}
	one := "/settings/sessions/" + otherID + "/revoke"
	all := "/settings/sessions/revoke-others"
	anonymous := env.newClient()
	anonymousCSRF := env.csrf(t, anonymous)
	anonymous.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for _, path := range []string{one, all} {
		post(anonymous, path, anonymousCSRF, http.StatusSeeOther)
		post(env.client, path, "wrong", http.StatusForbidden)
		post(env.client, path, "", http.StatusForbidden)
	}
	post(env.client, "/settings/sessions/"+outsiderID+"/revoke", csrf, 200)
	post(env.client, "/settings/sessions/"+currentID+"/revoke", csrf, 200)
	if sessionID(other) != otherID || sessionID(env.client) != currentID || sessionID(outsider) != outsiderID {
		t.Fatal("unauthorized revocation changed sessions")
	}
	// API credentials are independent of browser sessions.
	raw, hash, prefix := auth.NewToken()
	if err := env.store.CreateToken(ctx, &model.APIToken{ID: "session-test-token", UserID: user.ID, Name: "test", TokenHash: hash, TokenPrefix: prefix, Scopes: "read", CreatedAt: time.Now().Unix()}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{one, all} {
		req, _ := http.NewRequest(http.MethodPost, env.ts.URL+path, strings.NewReader(url.Values{"csrf": {anonymousCSRF}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Authorization", "Bearer "+raw)
		r, err := anonymous.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusForbidden {
			t.Fatalf("bearer revoke status=%d", r.StatusCode)
		}
	}
	assertAccess := func(c *http.Client, want int) {
		t.Helper()
		r, err := c.Get(env.ts.URL + "/api/v1/me")
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != want {
			t.Fatalf("session access=%d want=%d", r.StatusCode, want)
		}
	}
	post(env.client, one, csrf, 200)
	assertAccess(other, http.StatusUnauthorized)
	assertAccess(third, http.StatusOK)
	post(env.client, one, csrf, 200)
	post(env.client, all, csrf, 200)
	assertAccess(third, http.StatusUnauthorized)
	assertAccess(env.client, http.StatusOK)
	assertAccess(outsider, http.StatusOK)
	if _, _, err := env.store.TokenUser(ctx, hash); err != nil {
		t.Fatalf("session revocation revoked API token: %v", err)
	}
	logs, err := env.store.ListAudit(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, log := range logs {
		if strings.HasPrefix(log.Action, "user.session") {
			counts[log.Action]++
			if log.ActorID == nil || *log.ActorID != user.ID || (log.Target != otherID && log.Target != user.ID) {
				t.Fatal("incorrect session audit")
			}
		}
	}
	if counts["user.session_revoke"] != 1 || counts["user.sessions_revoke_others"] != 1 {
		t.Fatalf("session audit counts=%v", counts)
	}
}
