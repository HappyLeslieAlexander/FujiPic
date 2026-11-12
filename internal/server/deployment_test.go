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

// Exercise login through a proxy carrying the deployment's origin secret.
// ReverseProxy substitutes for Cloudflare's header transform plus Caddy;
// this does not exercise Docker, TLS, or the Cloudflare peer-IP gate.
func TestOriginAuthenticatedProxyLogin(t *testing.T) {
	const secret = "test-login-origin-secret"
	env := newTestEnv(t, func(c *config.Config) {
		c.OriginSecret = secret
	})
	target, err := url.Parse(env.ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	rp := httputil.NewSingleHostReverseProxy(target)
	direct := rp.Director
	rp.Director = func(r *http.Request) {
		direct(r)
		r.Header.Set("X-Origin-Secret", secret)
	}
	proxy := httptest.NewServer(rp)
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
		t.Fatalf("authenticated proxy GET /login = %d, want 200", resp.StatusCode)
	}
	proxyEnv.register(t, env.client, "readme-proxy@example.test", "password123", "")
	other := env.newClient()
	proxyEnv.login(t, other, "readme-proxy@example.test", "password123")
	t.Log("origin-authenticated proxy: GET /login, POST /register, POST /login and authenticated GET /dashboard passed")
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
		"\"443:8443/tcp\"",
		"S3_ENDPOINT: ${S3_ENDPOINT:-}",
		"S3_FORCE_PATH_STYLE: ${S3_FORCE_PATH_STYLE:-true}",
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

func TestCloudflareHostDeploymentContract(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile("../../" + path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	compose := read("deploy/compose.cloudflare-host.yml")
	for _, want := range []string{
		"name: fujipic-cloudflare", "- fujipic-data:/data",
		"\"127.0.0.1:18080:8080/tcp\"", "gateway: 172.30.84.1",
		"TRUSTED_PROXY_CIDRS: 172.30.84.1/32", "CLIENT_IP_HEADER: CF-Connecting-IP",
		"ORIGIN_SHARED_SECRET: ${ORIGIN_SHARED_SECRET:?", "CDN_PURGE_PROVIDER: cloudflare",
		"read_only: true", "cap_drop: [ALL]", "security_opt: [\"no-new-privileges:true\"]",
	} {
		if !strings.Contains(compose, want) {
			t.Errorf("host Compose missing %q", want)
		}
	}
	if strings.Contains(compose, "  caddy:") || strings.Contains(compose, "8443:") {
		t.Fatal("host deployment must not start a container proxy")
	}
	if strings.Count(compose, "18080:8080") != 1 || strings.Contains(compose, "network_mode:") {
		t.Fatal("app must have exactly one loopback-only port binding")
	}
	caddy := read("deploy/Caddyfile.cloudflare-host")
	for _, want := range []string{
		"https://{$APP_HOST}:443", "https://{$IMAGE_HOST}:443",
		"reverse_proxy 127.0.0.1:18080", "respond @not_cloudflare \"Forbidden\" 403",
		"path /i/*", "method GET HEAD", "request_header -Cookie", "request_header -Authorization",
	} {
		if !strings.Contains(caddy, want) {
			t.Errorf("host Caddy missing %q", want)
		}
	}
	if strings.Count(caddy, "import fujipic_host_gate") != 2 || strings.Contains(caddy, "header_up X-Origin-Secret") {
		t.Fatal("both hosts must use the source gate without injecting the origin secret")
	}
	// The legacy host variant keeps its OCI-specific endpoint/path-style
	// settings; the standalone variant also supports AWS defaults.
	old := read("deploy/compose.cloudflare.yml")
	settings := regexp.MustCompile(`(?m)^      ([A-Z][A-Z0-9_]*): (.*)$`)
	for _, match := range settings.FindAllStringSubmatch(old, -1) {
		if match[1] == "APP_HOST" || match[1] == "IMAGE_HOST" || match[1] == "TRUSTED_PROXY_CIDRS" || match[1] == "S3_ENDPOINT" || match[1] == "S3_FORCE_PATH_STYLE" {
			continue
		}
		if !strings.Contains(compose, match[0]) {
			t.Errorf("host Compose changed app setting %s", match[1])
		}
	}
}
