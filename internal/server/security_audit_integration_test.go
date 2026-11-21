package server_test

import (
	"fujipic/internal/config"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestLoginAttackerCannotLockDifferentNetwork(t *testing.T) {
	env := newTestEnv(t, func(c *config.Config) {
		c.TrustProxy = true
		c.TrustedProxyCIDRs = []string{"127.0.0.0/8"}
		c.ClientIPHeader = "X-Test-IP"
	})
	env.register(t, env.client, "victim@example.test", "password123", "")
	client := env.newClient()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	csrf := env.csrf(t, client)
	for i := 0; i < 7; i++ {
		password, ip := "wrong-password", "192.0.2.1"
		if i == 6 {
			password, ip = "password123", "198.51.100.1"
		}
		req, err := http.NewRequest("POST", env.ts.URL+"/login", strings.NewReader(url.Values{"csrf": {csrf}, "email": {"victim@example.test"}, "password": {password}}.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("X-Test-IP", ip)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if i == 6 && resp.StatusCode != 303 {
			t.Fatalf("victim locked: %d", resp.StatusCode)
		}
	}
}

func TestInvalidInviteDoesNotRevealExistingEmail(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t, env.client, "known@example.test", "password123", "")
	env.cfg.AllowOpenRegistration = false
	client := env.newClient()
	csrf := env.csrf(t, client)
	resp, err := client.PostForm(env.ts.URL+"/register", url.Values{"csrf": {csrf}, "email": {"known@example.test"}, "password": {"password123"}, "invite": {"invalid"}})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "该邮箱已注册") || !strings.Contains(string(body), "邀请码无效") {
		t.Fatal(string(body))
	}
}

func TestWebDeepPageRejected(t *testing.T) {
	env := newTestEnv(t, nil)
	_, client := adminSession(t, env)
	for _, path := range []string{"/dashboard", "/admin", "/admin/images"} {
		resp, err := client.Get(env.ts.URL + path + "?page=1001")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatal(path, resp.StatusCode)
		}
	}
}
