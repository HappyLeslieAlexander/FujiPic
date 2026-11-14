package server_test

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestAuthInputBoundaries(t *testing.T) {
	for _, path := range []string{"/login", "/register", "/logout"} {
		t.Run(path, func(t *testing.T) {
			env := newTestEnv(t, nil)
			csrf := env.csrf(t, env.client)
			req, _ := http.NewRequest("POST", env.ts.URL+path, strings.NewReader(strings.Repeat("x", 1<<20)))
			req.Header.Set("Content-Type", "multipart/form-data; boundary=test")
			resp, err := env.client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 413 {
				t.Fatalf("oversized body: %d", resp.StatusCode)
			}
			if path == "/logout" {
				return
			}
			resp, err = env.client.PostForm(env.ts.URL+path, url.Values{"csrf": {csrf}, "email": {strings.Repeat("z", 255)}, "password": {"test"}})
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 400 || strings.Contains(string(body), strings.Repeat("z", 255)) {
				t.Fatalf("oversized email echoed/accepted: %d", resp.StatusCode)
			}
		})
	}
}

func TestInvalidCSRFConsumesLoginIPLimit(t *testing.T) {
	env := newTestEnv(t, nil)
	for i := 0; i < 11; i++ {
		resp, err := env.client.PostForm(env.ts.URL+"/login", url.Values{"csrf": {"bad"}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		want := 403
		if i == 10 {
			want = 429
		}
		if resp.StatusCode != want {
			t.Fatalf("request %d: %d", i, resp.StatusCode)
		}
	}
}

func TestPrivateImageNeverServed(t *testing.T) {
	env := newTestEnv(t, nil)
	_, client := adminSession(t, env)
	_, img := env.upload(t, client, makePNG(t, 1, 1, false), "test.png", env.csrf(t, client), "")
	if _, err := env.db.Exec(`UPDATE images SET visibility='private' WHERE id=?`, img.ID); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(img.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("private image exposed: %d", resp.StatusCode)
	}
}

func TestAdminPaginationBeyondOldLimits(t *testing.T) {
	env := newTestEnv(t, nil)
	_, client := adminSession(t, env)
	for i := 0; i < 105; i++ {
		if _, err := env.db.Exec(`INSERT INTO audit_logs(id,action,created_at) VALUES(printf('old-%03d',?),'page-test',?)`, i, i); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := client.Get(env.ts.URL + "/admin?page=3")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "page-test") || !strings.Contains(string(body), "/admin?page=2") {
		t.Fatal("older audit page missing")
	}
	logs, err := env.store.ListAuditPage(context.Background(), 50, 100)
	if err != nil || len(logs) == 0 {
		t.Fatalf("logs=%d %v", len(logs), err)
	}
}
