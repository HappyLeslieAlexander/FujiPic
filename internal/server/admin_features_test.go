package server_test

import (
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"fujipic/internal/config"
	"fujipic/internal/model"
)

func TestMainlandOptimizedCopyOptions(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "primary-only", true: "optimized"}[enabled], func(t *testing.T) {
			env := newTestEnv(t, func(c *config.Config) {
				if enabled {
					c.PublicBaseAliases = []string{"https://cn.example.test", "https://cn2.example.test"}
				}
			})
			_, client := adminSession(t, env)
			_, img := env.upload(t, client, makePNG(t, 2, 2, false), `中文[a]&b.png`, env.csrf(t, client), "")
			alt := `中文\[a\]&b.png`
			for _, page := range []string{"/dashboard", "/admin/images"} {
				resp, err := client.Get(env.ts.URL + page)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil || resp.StatusCode != http.StatusOK {
					t.Fatalf("%s: %v status=%d", page, err, resp.StatusCode)
				}
				buttons := regexp.MustCompile(`<button[^>]*data-copy="([^"]*)"[^>]*>([^<]*)</button>`).FindAllStringSubmatch(string(body), -1)
				got := make(map[string][]string)
				for _, button := range buttons {
					got[button[2]] = append(got[button[2]], html.UnescapeString(button[1]))
				}
				want := map[string][]string{"链接": {img.URL}, "Markdown": {"![" + alt + "](" + img.URL + ")"}}
				for _, u := range img.AlternateURLs {
					want["中国大陆优化链接"] = append(want["中国大陆优化链接"], u)
					want["中国大陆优化 Markdown"] = append(want["中国大陆优化 Markdown"], "!["+alt+"]("+u+")")
				}
				if len(got) != len(want) {
					t.Fatalf("%s: unexpected copy options: %v", page, got)
				}
				for label, values := range want {
					if strings.Join(got[label], "\n") != strings.Join(values, "\n") {
						t.Errorf("%s %s: got %q want %q", page, label, got[label], values)
					}
				}
			}
		})
	}
}

func TestAdminInviteLimitsAndAuditClear(t *testing.T) {
	env := newTestEnv(t, nil)
	_, client := adminSession(t, env)
	csrf := env.csrf(t, client)
	for _, tc := range []struct {
		raw       string
		unlimited bool
		want      int64
	}{{"", false, 1}, {"3", false, 3}, {"", true, 0}} {
		form := url.Values{"csrf": {csrf}, "max_uses": {tc.raw}}
		if tc.unlimited {
			form.Set("unlimited", "1")
		}
		resp, err := client.PostForm(env.ts.URL+"/admin/invites", form)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		var n int
		if err := env.db.QueryRow(`SELECT count(*) FROM invites WHERE max_uses=?`, tc.want).Scan(&n); err != nil || n != 1 {
			t.Fatalf("invite limit=%d count=%d err=%v", tc.want, n, err)
		}
	}
	for _, raw := range []string{"-1", "0", "1.5", "1000001"} {
		r, err := client.PostForm(env.ts.URL+"/admin/invites", url.Values{"csrf": {csrf}, "max_uses": {raw}})
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
	}
	var n int
	if err := env.db.QueryRow(`SELECT count(*) FROM invites`).Scan(&n); err != nil || n != 3 {
		t.Fatal("invalid limits saved")
	}
	ordinary := env.newClient()
	env.register(t, ordinary, "ordinary@example.test", "password123", "")
	for _, tc := range []struct {
		c    *http.Client
		csrf string
	}{{ordinary, env.csrf(t, ordinary)}, {client, "invalid"}} {
		r, err := tc.c.PostForm(env.ts.URL+"/admin/audit/clear", url.Values{"csrf": {tc.csrf}})
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != 403 {
			t.Fatal("unauthorized clear allowed")
		}
	}
	logs, err := env.store.ListAudit(context.Background(), 100)
	if err != nil || len(logs) == 0 {
		t.Fatal("unauthorized request cleared audit")
	}
	resp, err := client.PostForm(env.ts.URL+"/admin/audit/clear", url.Values{"csrf": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	logs, err = env.store.ListAudit(context.Background(), 100)
	if err != nil || len(logs) != 0 {
		t.Fatal("clear failed")
	}
	if count, err := env.store.CountUsers(context.Background()); err != nil || count != 2 {
		t.Fatal("clear altered users")
	}
	if err := env.store.AddAudit(context.Background(), &model.AuditLog{ID: "after-clear", Action: "test"}); err != nil {
		t.Fatal(err)
	}
	logs, err = env.store.ListAudit(context.Background(), 100)
	if err != nil || len(logs) != 1 {
		t.Fatal("audit cannot resume")
	}
}

func TestImageAliasLinksReadsAndPurge(t *testing.T) {
	const alias = "https://alt.example.test"
	env := newTestEnv(t, func(c *config.Config) { c.PublicBaseAliases = []string{alias} })
	env.register(t, env.client, "alias@example.test", "password123", "")
	resp, img := env.upload(t, env.client, makePNG(t, 2, 2, false), "alias.png", env.csrf(t, env.client), "")
	if resp.StatusCode != 201 {
		t.Fatal("upload failed")
	}
	path := strings.TrimPrefix(img.URL, env.ts.URL)
	if len(img.AlternateURLs) != 1 || img.AlternateURLs[0] != alias+path {
		t.Fatal("upload omitted alternate URL")
	}
	for _, endpoint := range []string{"/api/v1/images/" + img.ID, "/api/v1/images"} {
		r, err := env.client.Get(env.ts.URL + endpoint)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if !strings.Contains(string(body), alias+path) {
			t.Fatalf("API omitted alternate URL: %s", endpoint)
		}
	}
	r, err := env.client.Get(env.ts.URL + "/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if !strings.Contains(string(body), `data-copy="`+alias+path+`"`) {
		t.Fatal("gallery lacks alternate URL")
	}
	for _, method := range []string{"GET", "HEAD"} {
		req, _ := http.NewRequest(method, env.ts.URL+path, nil)
		req.Host = "alt.example.test"
		req.Header.Set("Cookie", "irrelevant=1")
		req.Header.Set("Authorization", "Bearer invalid")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != 200 || r.Header.Get("Set-Cookie") != "" || !strings.Contains(r.Header.Get("Cache-Control"), "immutable") {
			t.Fatal("alias read contract failed")
		}
	}
	for _, endpoint := range []string{"/login", "/admin", "/api/v1/me"} {
		req, _ := http.NewRequest("GET", env.ts.URL+endpoint, nil)
		req.Host = "alt.example.test"
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != 404 || r.Header.Get("Set-Cookie") != "" {
			t.Fatal("alias exposed website")
		}
	}
	rangeReq, _ := http.NewRequest("GET", env.ts.URL+path, nil)
	rangeReq.Host = "alt.example.test"
	rangeReq.Header.Set("Range", "bytes=0-3")
	rangeResp, err := http.DefaultClient.Do(rangeReq)
	if err != nil {
		t.Fatal(err)
	}
	rangeBody, _ := io.ReadAll(rangeResp.Body)
	rangeResp.Body.Close()
	if rangeResp.StatusCode != 206 || len(rangeBody) != 4 {
		t.Fatal("alias Range failed")
	}
	req, _ := http.NewRequest("POST", env.ts.URL+path, nil)
	req.Host = "alt.example.test"
	r, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 404 {
		t.Fatal("alias accepted write")
	}
	// URLs must never be derived from a forged request host.
	req, _ = http.NewRequest("GET", env.ts.URL+"/api/v1/images/"+img.ID, nil)
	req.Host = "untrusted.example.test"
	for _, cookie := range env.client.Jar.Cookies(req.URL) {
		req.AddCookie(cookie)
	}
	r, err = env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var detail struct {
		URL           string
		AlternateURLs []string `json:"alternate_urls"`
	}
	if err := json.NewDecoder(r.Body).Decode(&detail); err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if detail.URL != img.URL || len(detail.AlternateURLs) != 1 || detail.AlternateURLs[0] != alias+path {
		t.Fatal("host influenced generated URL")
	}
	_, admin := adminSession(t, env)
	r, err = admin.PostForm(env.ts.URL+"/admin/images/"+img.ID+"/delete", url.Values{"csrf": {env.csrf(t, admin)}})
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	rows, err := env.db.Query(`SELECT url FROM cdn_purges ORDER BY url`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	urls := map[string]bool{}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			t.Fatal(err)
		}
		urls[u] = true
	}
	if len(urls) != 2 || !urls[img.URL] || !urls[alias+path] {
		t.Fatalf("purge missing domain: %v", urls)
	}
	req, _ = http.NewRequest("GET", env.ts.URL+path, nil)
	req.Host = "alt.example.test"
	r, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	r.Body.Close()
	if r.StatusCode != 404 {
		t.Fatal("deleted image still served")
	}
}
