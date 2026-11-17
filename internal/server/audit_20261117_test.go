package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"fujipic/internal/auth"
	"fujipic/internal/config"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPrimaryImageHostIsolationAndSharedHostCompatibility(t *testing.T) {
	s := &Server{cfg: &config.Config{AppBaseURL: "https://app.test", PublicBaseURL: "https://img.test", PublicBaseAliases: []string{"https://alias.test"}}}
	called := 0
	h := s.withImageAlias(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called++; w.WriteHeader(204) }))
	for _, host := range []string{"img.test", "IMG.TEST.:443", "alias.test"} {
		for _, path := range []string{"/login", "/dashboard", "/api/v1/tokens"} {
			r := httptest.NewRequest("GET", "https://"+host+path, nil)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 404 || w.Header().Get("Set-Cookie") != "" || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal(host, path, w.Code, w.Header())
			}
		}
	}
	if called != 0 {
		t.Fatal("image host reached auth/routes")
	}
	for _, method := range []string{"GET", "HEAD"} {
		r := httptest.NewRequest(method, "https://img.test/i/test.png", nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 204 {
			t.Fatal(w.Code)
		}
	}
	s.cfg.PublicBaseURL = s.cfg.AppBaseURL
	h = s.withImageAlias(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "https://app.test/login", nil))
	if w.Code != 204 {
		t.Fatal("same-host deployment broken")
	}
}

func TestOuterAccessLogCapturesMiddlewareRejections(t *testing.T) {
	for _, tc := range []struct {
		path, host, secret string
		size               int64
		want               int
	}{
		{"/login", "app.test", "", 0, 403},
		{"/login", "img.test", "secret", 0, 404},
		{"/login", "app.test", "secret", 1 << 20, 413},
	} {
		var logs bytes.Buffer
		s := &Server{cfg: &config.Config{AppBaseURL: "https://app.test", PublicBaseURL: "https://img.test", OriginSecret: "secret"}, log: slog.New(slog.NewJSONHandler(&logs, nil))}
		r := httptest.NewRequest("POST", "https://"+tc.host+tc.path, nil)
		r.ContentLength = tc.size
		r.Header.Set("X-Origin-Secret", tc.secret)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatal(w.Code)
		}
		var entry map[string]any
		if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
			t.Fatal(err, logs.String())
		}
		if entry["status"] != float64(tc.want) || entry["request_id"] == "" {
			t.Fatal(entry)
		}
	}
}

func TestOuterAccessLogRecordsRecoveredPanicStatus(t *testing.T) {
	var logs bytes.Buffer
	s := &Server{cfg: &config.Config{}, log: slog.New(slog.NewJSONHandler(&logs, nil))}
	h := s.withRequestID(s.withAccessLog(s.withRecovery(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("test panic") }))))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/panic", nil))
	if w.Code != 500 {
		t.Fatal(w.Code)
	}
	decoder := json.NewDecoder(&logs)
	var recovered, access map[string]any
	if err := decoder.Decode(&recovered); err != nil {
		t.Fatal(err)
	}
	if err := decoder.Decode(&access); err != nil {
		t.Fatal(err)
	}
	if access["msg"] != "request" || access["status"] != float64(500) || access["request_id"] == "" {
		t.Fatal(access)
	}
}

func TestLimiterBudgetsDoNotCollide(t *testing.T) {
	l := newLimiter()
	for i := 0; i < 10000; i++ {
		key := fmt.Sprintf("login:%d", i)
		if !l.allow(key, 1, time.Minute) {
			t.Fatal("independent key rejected", i)
		}
		if l.allow(key, 1, time.Minute) {
			t.Fatal("limit bypass", i)
		}
	}
	for i := 10000; i < maxLimitKeys; i++ {
		if !l.allow(fmt.Sprintf("login:%d", i), 1, time.Minute) {
			t.Fatal(i)
		}
	}
	if l.allow("login:overflow", 1, time.Minute) {
		t.Fatal("capacity exceeded")
	}
	if l.allow("login:0", 1, time.Minute) {
		t.Fatal("live budget evicted")
	}
	if !l.allow("upload:other", 1, time.Minute) {
		t.Fatal("other policy blocked")
	}
}

func TestPasswordBusyUsesLocalizedWebFlash(t *testing.T) {
	s := &Server{cfg: &config.Config{AppEnv: "development"}}
	for lang, want := range map[string]string{
		"en":      "Too many requests. Please try again later.",
		"zh-Hans": "请求过于频繁，请稍后再试",
		"zh-Hant": "請求過於頻繁，請稍後再試。",
	} {
		w := httptest.NewRecorder()
		w.Header().Set("Content-Language", lang)
		r := httptest.NewRequest("POST", "/login", nil)
		if !s.passwordBusy(w, r, auth.ErrBusy) || w.Code != 303 || w.Header().Get("Location") != "/login" || w.Header().Get("Retry-After") != "2" {
			t.Fatal(w.Code, w.Header())
		}
		if len(w.Result().Cookies()) != 1 {
			t.Fatal("missing flash")
		}
		message, err := base64.RawURLEncoding.DecodeString(w.Result().Cookies()[0].Value)
		if err != nil || string(message) != want {
			t.Fatal(lang, string(message), err)
		}
	}
}
