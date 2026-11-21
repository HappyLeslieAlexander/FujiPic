package server_test

import (
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"fujipic/internal/config"
)

func TestLocalizedPagesAndErrors(t *testing.T) {
	env := newTestEnv(t, func(c *config.Config) {
		c.TurnstileSiteKey = "public-test-key"
		c.TurnstileSecret = "private-test-secret"
		c.TurnstileHostnames = []string{"example.test"}
	})
	for _, tc := range []struct{ header, lang, title, errorText string }{
		{"en-US", "en", "Sign in", "CSRF verification failed"},
		{"zh-CN", "zh-Hans", "登录", "CSRF 校验失败"},
		{"zh-TW", "zh-Hant", "登入", "CSRF 驗證失敗"},
		{"zh-HK", "zh-Hant", "登入", "CSRF 驗證失敗"},
		{"fr-FR,zh-CN;q=0.5", "en", "Sign in", "CSRF verification failed"},
		{"de", "en", "Sign in", "CSRF verification failed"},
		{"ja", "en", "Sign in", "CSRF verification failed"},
		{"", "en", "Sign in", "CSRF verification failed"},
	} {
		t.Run(tc.header, func(t *testing.T) {
			for _, path := range []string{"/login", "/register"} {
				req, _ := http.NewRequest("GET", env.ts.URL+path, nil)
				req.Header.Set("Accept-Language", tc.header)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				body, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != 200 || resp.Header.Get("Content-Language") != tc.lang || !strings.Contains(resp.Header.Get("Vary"), "Accept-Language") || resp.Header.Get("Cache-Control") != "no-store" {
					t.Fatalf("unexpected response: %d %v", resp.StatusCode, resp.Header)
				}
				widgetLang := map[string]string{"en": "en", "zh-Hans": "zh-cn", "zh-Hant": "zh-tw"}[tc.lang]
				for _, want := range []string{`lang="` + tc.lang + `"`, `data-language="` + widgetLang + `"`, tc.title} {
					if !strings.Contains(string(body), want) {
						t.Errorf("missing %q", want)
					}
				}
				match := regexp.MustCompile(`<meta name="fujipic-messages" content="([^"]+)"`).FindSubmatch(body)
				if len(match) != 2 {
					t.Fatal("missing client messages")
				}
				var messages map[string]string
				if err := json.Unmarshal([]byte(html.UnescapeString(string(match[1]))), &messages); err != nil {
					t.Fatal(err)
				}
				want := map[string]string{"en": "Upload successful", "zh-Hans": "上传成功", "zh-Hant": "上傳成功"}[tc.lang]
				if messages["上传成功"] != want {
					t.Fatalf("client locale mismatch: %v", messages)
				}
			}
			req, _ := http.NewRequest("POST", env.ts.URL+"/login", strings.NewReader(""))
			req.Header.Set("Accept-Language", tc.header)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 403 || !strings.Contains(string(body), tc.errorText) {
				t.Fatalf("untranslated HTTP error: %d %s", resp.StatusCode, body)
			}
			req, _ = http.NewRequest("GET", env.ts.URL+"/api/v1/me", nil)
			req.Header.Set("Accept-Language", tc.header)
			resp, err = http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			var payload struct {
				Error struct{ Code, Message string }
			}
			err = json.NewDecoder(resp.Body).Decode(&payload)
			resp.Body.Close()
			want := map[string]string{"en": "Sign-in or a valid API token is required", "zh-Hans": "需要登录或有效的 API Token", "zh-Hant": "需要登入或有效的 API Token"}[tc.lang]
			if err != nil || resp.StatusCode != 401 || payload.Error.Code != "unauthorized" || payload.Error.Message != want {
				t.Fatalf("localized API error: %+v, %v", payload, err)
			}
		})
	}
}

func TestLocalizedAuthenticatedPagesAndFlash(t *testing.T) {
	env := newTestEnv(t, nil)
	client := env.client
	env.register(t, client, "i18n@example.com", "password123", "")
	if _, err := env.db.Exec("UPDATE users SET role='admin' WHERE email='i18n@example.com'"); err != nil {
		t.Fatal(err)
	}
	// User-controlled text resembling a translation key must never be translated.
	_, img := env.upload(t, client, makePNG(t, 2, 2, false), "登录.png", env.csrf(t, client), "")
	for _, tc := range []struct{ lang, gallery, settings, admin string }{
		{"en", "My gallery", "Login sessions", "Administration"},
		{"zh-Hant", "我的圖庫", "登入工作階段", "管理後台"},
		{"zh-Hans", "我的图库", "登录会话", "管理后台"},
	} {
		for path, want := range map[string]string{"/dashboard": tc.gallery, "/settings": tc.settings, "/admin": tc.admin, "/admin/images": "登录.png"} {
			req, _ := http.NewRequest("GET", env.ts.URL+path, nil)
			req.Header.Set("Accept-Language", tc.lang)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || !strings.Contains(string(body), want) || strings.Contains(string(body), "template error") {
				t.Fatalf("%s (%s): %d missing %q", path, tc.lang, resp.StatusCode, want)
			}
		}
	}
	csrf := env.csrf(t, client)
	req, _ := http.NewRequest("POST", env.ts.URL+"/admin/audit/clear", strings.NewReader(url.Values{"csrf": {csrf}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept-Language", "zh-Hant")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "稽核記錄已清空。") {
		t.Fatalf("flash was not localized: %s", body)
	}
	// The same public image and static files stay language-neutral and cacheable.
	for _, path := range []string{strings.TrimPrefix(img.URL, env.ts.URL), "/static/app.js", "/static/app.css", "/healthz", "/readyz"} {
		var previous []byte
		for _, lang := range []string{"en", "zh-Hant"} {
			req, _ := http.NewRequest("GET", env.ts.URL+path, nil)
			req.Header.Set("Accept-Language", lang)
			req.AddCookie(&http.Cookie{Name: "fujipic_language", Value: "zh-Hans"})
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || resp.Header.Get("Content-Language") != "" || strings.Contains(resp.Header.Get("Vary"), "Accept-Language") || len(resp.Cookies()) != 0 {
				t.Fatalf("language-neutral path changed: %s %v", path, resp.Header)
			}
			if previous != nil && string(body) != string(previous) {
				t.Fatalf("localized bytes on %s", path)
			}
			previous = body
		}
	}
}

func TestConcurrentLanguageIsolation(t *testing.T) {
	env := newTestEnv(t, nil)
	var wg sync.WaitGroup
	for i := 0; i < 18; i++ {
		lang := []string{"en", "zh-Hans", "zh-Hant"}[i%3]
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("GET", env.ts.URL+"/login", nil)
			req.Header.Set("Accept-Language", lang)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), `<html lang="`+lang+`">`) {
				t.Errorf("request locale leaked: %s", lang)
			}
		}()
	}
	wg.Wait()
}

func TestManualLanguagePreference(t *testing.T) {
	env := newTestEnv(t, nil)
	client := *env.client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	csrf := env.csrf(t, env.client)
	post := func(lang, token, target string) *http.Response {
		t.Helper()
		resp, err := client.PostForm(env.ts.URL+"/language", url.Values{"language": {lang}, "csrf": {token}, "return_to": {target}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	for _, lang := range []string{"zh-Hant", "zh-Hans", "en"} {
		resp := post(lang, csrf, "/login?next=gallery&test=1")
		if resp.StatusCode != 303 || resp.Header.Get("Location") != "/login?next=gallery&test=1" {
			t.Fatalf("switch redirect: %d %v", resp.StatusCode, resp.Header)
		}
		var preference *http.Cookie
		for _, c := range resp.Cookies() {
			if c.Name == "fujipic_language" {
				preference = c
			}
		}
		if preference == nil || preference.Value != lang || preference.MaxAge != 365*24*3600 || !preference.HttpOnly || preference.Path != "/" || preference.SameSite != http.SameSiteLaxMode || preference.Expires.IsZero() {
			t.Fatalf("invalid persistent cookie: %+v", preference)
		}
		for _, path := range []string{"/login", "/register", "/api/v1/me"} {
			req, _ := http.NewRequest("GET", env.ts.URL+path, nil)
			req.Header.Set("Accept-Language", "zh-TW")
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.Header.Get("Content-Language") != lang || resp.Header.Get("Cache-Control") != "no-store" || !strings.Contains(strings.Join(resp.Header.Values("Vary"), ","), "Cookie") {
				t.Fatalf("manual language lost on %s: %v", path, resp.Header)
			}
			if path == "/login" && (!strings.Contains(string(body), `<html lang="`+lang+`">`) || !strings.Contains(string(body), `value="`+lang+`" aria-current="true"`)) {
				t.Fatal("language selection not reflected in page")
			}
		}
	}
	for _, tc := range []struct {
		lang, token string
		status      int
	}{{"fr", csrf, 400}, {"zh-CN", csrf, 400}, {"zh-Hant", "wrong", 403}} {
		resp := post(tc.lang, tc.token, "/login")
		if resp.StatusCode != tc.status || len(resp.Cookies()) != 0 {
			t.Fatalf("invalid preference accepted: %d %v", resp.StatusCode, resp.Cookies())
		}
	}
	for _, target := range []string{"https://example.test", "//example.test", "/\\example.test", "/%2fexample.test", "/logout", "/language", "/login\r\nX-Test: bad"} {
		if resp := post("en", csrf, target); resp.Header.Get("Location") != "/" {
			t.Fatalf("unsafe redirect: %q", resp.Header.Get("Location"))
		}
	}
	resp := post("auto", csrf, "/login")
	if resp.StatusCode != 303 || resp.Cookies()[0].MaxAge != -1 {
		t.Fatal("browser preference not restored")
	}
	for _, invalid := range []string{"", "fr", "zh-CN"} {
		req, _ := http.NewRequest("GET", env.ts.URL+"/login", nil)
		req.Header.Set("Accept-Language", "zh-TW")
		req.AddCookie(&http.Cookie{Name: "fujipic_language", Value: invalid})
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.Header.Get("Content-Language") != "zh-Hant" {
			t.Fatal("invalid cookie blocked browser language")
		}
	}
	_, adminClient := adminSession(t, env)
	adminCSRF := env.csrf(t, adminClient)
	resp, err := adminClient.PostForm(env.ts.URL+"/language", url.Values{"language": {"zh-Hant"}, "csrf": {adminCSRF}, "return_to": {"/admin/images?user_search=example&page=1"}})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Language") != "zh-Hant" || resp.Request.URL.RequestURI() != "/admin/images?user_search=example&page=1" {
		t.Fatalf("authenticated return lost: %d %s", resp.StatusCode, resp.Request.URL)
	}
}
