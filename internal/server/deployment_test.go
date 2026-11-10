package server_test

import (
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"

	"fujipic/internal/config"
)

// Exercise the README's loopback-only proxy contract with real HTTP requests.
// This is not a Caddy/TLS/systemd deployment test: ReverseProxy substitutes for
// the documented proxy, whose default configuration adds no origin secret.
func TestReadmeLoopbackProxyLogin(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	secret := regexp.MustCompile(`(?m)^ORIGIN_SHARED_SECRET=(.*)$`).FindSubmatch(readme)
	if secret == nil {
		t.Fatal("README production environment must explicitly configure ORIGIN_SHARED_SECRET")
	}
	caddy := regexp.MustCompile("(?s)```caddy\n(.*?)\n```").FindSubmatch(readme)
	if caddy == nil || !strings.Contains(string(caddy[1]), "reverse_proxy 127.0.0.1:8080") {
		t.Fatal("README must document the loopback proxy used by this test")
	}
	if !regexp.MustCompile(`(?m)^LISTEN_ADDR=127\.0\.0\.1:8080$`).Match(readme) {
		t.Fatal("README must bind the unprotected origin to loopback")
	}
	env := newTestEnv(t, func(c *config.Config) {
		c.OriginSecret = strings.TrimSpace(string(secret[1]))
	})
	target, err := url.Parse(env.ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(httputil.NewSingleHostReverseProxy(target))
	defer proxy.Close()
	env.cfg.AppBaseURL = proxy.URL
	// Existing registration/login helpers now send requests through the proxy.
	proxyEnv := *env
	proxyEnv.ts = proxy
	resp, err := env.client.Get(proxy.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("README proxy GET /login = %d, want 200; origin secret requires matching proxy injection", resp.StatusCode)
	}
	proxyEnv.register(t, env.client, "readme-proxy@example.test", "password123", "")
	other := env.newClient()
	proxyEnv.login(t, other, "readme-proxy@example.test", "password123")
	t.Log("README loopback proxy: GET /login, POST /register, POST /login and authenticated GET /dashboard passed")
}

func TestProxyOriginSecretInjection(t *testing.T) {
	const secret = "test-origin-secret"
	env := newTestEnv(t, func(c *config.Config) { c.OriginSecret = secret })
	target, err := url.Parse(env.ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, inject := range []bool{false, true} {
		name := "without injection"
		if inject {
			name = "matching injection"
		}
		t.Run(name, func(t *testing.T) {
			rp := httputil.NewSingleHostReverseProxy(target)
			if inject {
				direct := rp.Director
				rp.Director = func(r *http.Request) {
					direct(r)
					// Overwrite, never append to a caller-supplied value.
					r.Header.Set("X-Origin-Secret", secret)
				}
			}
			proxy := httptest.NewServer(rp)
			defer proxy.Close()
			for _, tc := range []struct {
				path   string
				status int
			}{
				{"/healthz", http.StatusOK},
				{"/readyz", http.StatusOK},
				{"/login", http.StatusOK},
				{"/static/app.js", http.StatusOK},
				{"/api/v1/me", http.StatusUnauthorized},
				{"/i/2026/11/aaaaaaaaaaaaaaaaaaaaaaaaaa.png", http.StatusNotFound},
			} {
				req, err := http.NewRequest(http.MethodGet, proxy.URL+tc.path, nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("X-Origin-Secret", "untrusted-client-value")
				resp, err := proxy.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				resp.Body.Close()
				want := tc.status
				if !inject && tc.path != "/healthz" && tc.path != "/readyz" {
					want = http.StatusForbidden
				}
				if resp.StatusCode != want {
					t.Errorf("GET %s = %d, want %d", tc.path, resp.StatusCode, want)
				}
			}
		})
	}
}
