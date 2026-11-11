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

// Static guards complement scripts/test-cloudflare-proxy.py, which exercises
// the actual Caddy routing with a native binary. Neither deploys Docker/OCI.
func TestCloudflareDeploymentContract(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile("../../" + path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	compose := read("deploy/compose.cloudflare.yml")
	for _, want := range []string{
		"APP_BASE_URL: https://${APP_HOST:?set APP_HOST}",
		"PUBLIC_BASE_URL: https://${IMAGE_HOST:?set IMAGE_HOST}",
		"TRUSTED_PROXY_CIDRS: 172.30.84.2/32",
		"ipv4_address: 172.30.84.2",
		"CLIENT_IP_HEADER: CF-Connecting-IP",
		"ORIGIN_SHARED_SECRET: ${ORIGIN_SHARED_SECRET:?",
		"CDN_PURGE_PROVIDER: cloudflare",
		"ALLOW_OPEN_REGISTRATION: \"false\"",
		"\"8443:8443/tcp\"",
	} {
		if !strings.Contains(compose, want) {
			t.Errorf("Cloudflare Compose missing %q", want)
		}
	}
	if len(regexp.MustCompile(`(?m)^    ports:$`).FindAllString(compose, -1)) != 1 {
		t.Fatal("review unexpected published port declarations")
	}
	if strings.Contains(compose, "8080:8080") || strings.Contains(compose, "/var/run/docker.sock") {
		t.Fatal("app port or Docker socket must not be published")
	}
	for _, want := range []string{"read_only: true", "cap_drop: [ALL]", "security_opt: [\"no-new-privileges:true\"]", "user: \"65532:65532\""} {
		if len(regexp.MustCompile(`(?m)^    `+regexp.QuoteMeta(want)+`$`).FindAllString(compose, -1)) != 2 {
			t.Errorf("both containers must use %q", want)
		}
	}
	caddy := read("deploy/Caddyfile.cloudflare")
	for _, want := range []string{
		"https://{$APP_HOST}:8443", "https://{$IMAGE_HOST}:8443",
		"tls /etc/caddy/origin/cert.pem /etc/caddy/origin/key.pem",
		"respond @not_cloudflare \"Forbidden\" 403",
		"path /i/*", "method GET HEAD", "request_header -Cookie",
		"request_header -Authorization", "admin off", "auto_https off",
	} {
		if !strings.Contains(caddy, want) {
			t.Errorf("Cloudflare Caddyfile missing %q", want)
		}
	}
	if strings.Count(caddy, "import origin_gate") != 2 || strings.Contains(caddy, "header_up X-Origin-Secret") {
		t.Fatal("both hosts must use the origin gate; only Cloudflare may inject the secret")
	}
	if !strings.Contains(read("deploy/cloudflare-ips.caddy"), "not remote_ip ") {
		t.Fatal("source gate must use the actual TCP peer, not a forwarded IP")
	}
}
