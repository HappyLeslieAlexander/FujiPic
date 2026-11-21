package server_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"fujipic/internal/auth"
	"fujipic/internal/config"
	"fujipic/internal/model"
	"fujipic/internal/server"
)

type turnstileTransport func(*http.Request) (*http.Response, error)

func (f turnstileTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTurnstileFormGates(t *testing.T) {
	for _, path := range []string{"/login", "/register"} {
		t.Run(path, func(t *testing.T) {
			action := "login"
			if path == "/register" {
				action = "signup"
			}
			for _, tc := range []struct {
				name, token, response string
				status                int
				transportErr, allowed bool
			}{
				{"missing", "", "", 200, false, false},
				{"oversize", strings.Repeat("x", 2049), "", 200, false, false},
				{"invalid", "test-token", `{"success":false}`, 200, false, false},
				{"wrong-host", "test-token", `{"success":true,"hostname":"evil.example","action":"` + action + `"}`, 200, false, false},
				{"wrong-action", "test-token", `{"success":true,"hostname":"pic.example.test","action":"other"}`, 200, false, false},
				{"missing-fields", "test-token", `{"success":true}`, 200, false, false},
				{"malformed", "test-token", `<html>unavailable</html>`, 200, false, false},
				{"oversize-response", "test-token", strings.Repeat("x", 16*1024+1), 200, false, false},
				{"http-error", "test-token", `{}`, 503, false, false},
				{"redirect", "test-token", `{}`, 307, false, false},
				{"network", "test-token", "", 200, true, false},
				{"success", "test-token", `{"success":true,"hostname":"pic.example.test","action":"` + action + `"}`, 200, false, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var mu sync.Mutex
					calls := 0
					env := newTestEnvWithServer(t, func(c *config.Config) {
						c.TurnstileSiteKey = "public-test-site"
						c.TurnstileSecret = "private-test-secret"
						c.TurnstileHostnames = []string{"pic.example.test"}
					}, slog.NewTextHandler(io.Discard, nil), func(s *server.Server) {
						s.SetTurnstileTransport(turnstileTransport(func(r *http.Request) (*http.Response, error) {
							mu.Lock()
							calls++
							mu.Unlock()
							body, _ := io.ReadAll(r.Body)
							form, _ := url.ParseQuery(string(body))
							if r.URL.String() != "https://challenges.cloudflare.com/turnstile/v0/siteverify" || r.Method != "POST" || form.Get("secret") != "private-test-secret" || form.Get("response") != tc.token || form.Get("remoteip") != "127.0.0.1" {
								t.Error("incorrect Siteverify request")
							}
							if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > 10*time.Second {
								t.Error("missing verification deadline")
							}
							if tc.transportErr {
								return nil, errors.New("network unavailable")
							}
							return &http.Response{StatusCode: tc.status, Header: http.Header{"Location": {"https://evil.example/"}}, Body: io.NopCloser(strings.NewReader(tc.response)), Request: r}, nil
						}))
					})
					if path == "/login" {
						hash, err := auth.HashPassword("password123")
						if err != nil {
							t.Fatal(err)
						}
						if err := env.store.CreateUser(context.Background(), &model.User{ID: "user", Email: "turnstile@example.test", PasswordHash: hash, Role: "user", Status: "active"}); err != nil {
							t.Fatal(err)
						}
					}
					csrf := env.csrf(t, env.client)
					env.client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
					r, err := env.client.PostForm(env.ts.URL+path, url.Values{"csrf": {csrf}, "email": {"turnstile@example.test"}, "password": {"password123"}, "cf-turnstile-response": {tc.token}})
					if err != nil {
						t.Fatal(err)
					}
					body, _ := io.ReadAll(r.Body)
					r.Body.Close()
					want := http.StatusForbidden
					if tc.allowed {
						want = http.StatusSeeOther
					}
					if r.StatusCode != want {
						t.Fatalf("status=%d want=%d", r.StatusCode, want)
					}
					if strings.Contains(string(body), "private-test-secret") || strings.Contains(string(body), "test-token") {
						t.Fatal("render exposed credentials")
					}
					if !tc.allowed && (!strings.Contains(string(body), `data-action="`+action+`"`) || !strings.Contains(string(body), "人机验证未通过")) {
						t.Fatal("failed request did not render fresh challenge")
					}
					var sessions int
					if err := env.db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions); err != nil || (sessions == 1) != tc.allowed {
						t.Fatalf("session mutation=%d err=%v", sessions, err)
					}
					if path == "/register" {
						count, err := env.store.CountUsers(context.Background())
						if err != nil || (count == 1) != tc.allowed {
							t.Fatalf("user mutation=%d err=%v", count, err)
						}
					}
					mu.Lock()
					defer mu.Unlock()
					wantCalls := 1
					if tc.name == "missing" || tc.name == "oversize" {
						wantCalls = 0
					}
					if calls != wantCalls {
						t.Fatalf("siteverify calls=%d want=%d", calls, wantCalls)
					}
				})
			}
		})
	}
}

func TestTurnstileRenderingAndCSP(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		env := newTestEnv(t, func(c *config.Config) {
			if enabled {
				c.TurnstileSiteKey = "public-site-key"
				c.TurnstileSecret = "private-secret"
				c.TurnstileHostnames = []string{"pic.example.test"}
			}
		})
		for _, path := range []string{"/login", "/register", "/healthz"} {
			r, err := env.client.Get(env.ts.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(r.Body)
			r.Body.Close()
			want := enabled && path != "/healthz"
			if strings.Contains(string(body), "challenges.cloudflare.com") != want || strings.Contains(r.Header.Get("Content-Security-Policy"), "https://challenges.cloudflare.com") != want {
				t.Fatalf("widget/CSP mismatch enabled=%v path=%s", enabled, path)
			}
			if strings.Contains(string(body), "private-secret") || strings.Contains(r.Header.Get("Content-Security-Policy"), "unsafe-") {
				t.Fatal("secret exposure or unsafe CSP")
			}
		}
	}
}

func TestTurnstileReplayCSRFAndRateLimit(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	seen := map[string]bool{}
	env := newTestEnvWithServer(t, func(c *config.Config) {
		c.TurnstileSiteKey = "public-site-key"
		c.TurnstileSecret = "private-secret"
		c.TurnstileHostnames = []string{"pic.example.test"}
	}, slog.NewTextHandler(io.Discard, nil), func(s *server.Server) {
		s.SetTurnstileTransport(turnstileTransport(func(r *http.Request) (*http.Response, error) {
			mu.Lock()
			defer mu.Unlock()
			calls++
			body, _ := io.ReadAll(r.Body)
			form, _ := url.ParseQuery(string(body))
			token := form.Get("response")
			response := `{"success":true,"hostname":"pic.example.test","action":"login"}`
			if seen[token] {
				response = `{"success":false,"error-codes":["timeout-or-duplicate"]}`
			}
			seen[token] = true
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response)), Request: r}, nil
		}))
	})
	hash, err := auth.HashPassword("password123")
	if err != nil {
		t.Fatal(err)
	}
	if err := env.store.CreateUser(context.Background(), &model.User{ID: "user", Email: "replay@example.test", PasswordHash: hash, Role: "user", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	post := func(path string, values url.Values, want int) {
		t.Helper()
		c := env.newClient()
		csrf := env.csrf(t, c)
		if !values.Has("csrf") {
			values.Set("csrf", csrf)
		}
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		r, err := c.PostForm(env.ts.URL+path, values)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != want {
			t.Fatalf("POST status=%d want=%d", r.StatusCode, want)
		}
	}
	for _, path := range []string{"/login", "/register"} {
		post(path, url.Values{"csrf": {"bad"}, "email": {"csrf@example.test"}, "password": {"password123"}, "cf-turnstile-response": {"unused"}}, 403)
	}
	post("/login", url.Values{"email": {"duplicate@example.test"}, "password": {"password123"}, "cf-turnstile-response": {"a", "b"}}, 403)
	mu.Lock()
	if calls != 0 {
		t.Fatal("invalid CSRF or duplicate fields reached Siteverify")
	}
	mu.Unlock()
	post("/login", url.Values{"email": {"replay@example.test"}, "password": {"wrong-password"}, "cf-turnstile-response": {"consumed"}}, 200)
	post("/login", url.Values{"email": {"replay@example.test"}, "password": {"password123"}, "cf-turnstile-response": {"consumed"}}, 403)
	post("/login", url.Values{"email": {"replay@example.test"}, "password": {"password123"}, "cf-turnstile-response": {"fresh"}}, 303)
	post("/login", url.Values{"email": {"replay@example.test"}, "password": {"password123"}, "cf-turnstile-response": {"fresh"}}, 403)
	post("/login", url.Values{"email": {"replay@example.test"}, "password": {"wrong-password"}, "cf-turnstile-response": {"last-allowed"}}, 200)
	post("/login", url.Values{"email": {"replay@example.test"}, "password": {"password123"}, "cf-turnstile-response": {"rate-limited"}}, 303)
	mu.Lock()
	defer mu.Unlock()
	if calls != 6 || !seen["rate-limited"] {
		t.Fatalf("rate limiter failed; calls=%d", calls)
	}
	var sessions int
	if err := env.db.QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions); err != nil || sessions != 2 {
		t.Fatalf("replayed or rate-limited request created a session: %d %v", sessions, err)
	}
}
