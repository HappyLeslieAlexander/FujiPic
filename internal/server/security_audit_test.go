package server

import (
	"bytes"
	"context"
	"fujipic/internal/config"
	"fujipic/internal/model"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUploadIsolationAndEarlyOriginRejection(t *testing.T) {
	s := &Server{cfg: &config.Config{TmpDir: t.TempDir(), MaxUploadBytes: 1 << 20, AppBaseURL: "https://app.test"}, uploadSlots: make(chan struct{}, 8), limiter: newLimiter()}
	release1, ok := s.acquireUserUpload(httptest.NewRecorder(), "a")
	if !ok {
		t.Fatal("first upload rejected")
	}
	defer release1()
	release2, ok := s.acquireUserUpload(httptest.NewRecorder(), "a")
	if !ok {
		t.Fatal("second upload rejected")
	}
	defer release2()
	if _, ok := s.acquireUserUpload(httptest.NewRecorder(), "a"); ok {
		t.Fatal("user monopolized slots")
	}
	release3, ok := s.acquireUserUpload(httptest.NewRecorder(), "b")
	if !ok {
		t.Fatal("other user blocked")
	}
	defer release3()
	for _, tc := range []struct{ origin, cookie, token string }{
		{"https://evil.test", "valid", ""},
		{"https://app.test", "", ""},
		{"https://app.test", "valid", "wrong"},
	} {
		body := &countedBody{Reader: strings.NewReader(strings.Repeat("x", 4096))}
		r := httptest.NewRequest("POST", "https://app.test/upload", nil)
		r.Body = body
		r.Header.Set("Origin", tc.origin)
		if tc.cookie != "" {
			r.AddCookie(&http.Cookie{Name: csrfCookie, Value: tc.cookie})
		}
		r.Header.Set("X-CSRF-Token", tc.token)
		r = r.WithContext(context.WithValue(r.Context(), ctxAuth, &authInfo{User: &model.User{ID: "c"}}))
		w := httptest.NewRecorder()
		s.handleWebUpload(w, r)
		if w.Code != 403 || body.read != 0 || len(s.uploadSlots) != 3 {
			t.Fatalf("status=%d reads=%d slots=%d", w.Code, body.read, len(s.uploadSlots))
		}
	}
}

func TestMutationLimitsSharedAcrossTokenSurfaces(t *testing.T) {
	s := &Server{limiter: newLimiter()}
	called := 0
	h := s.withRequestLimits(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called++; w.WriteHeader(204) }))
	for i := 0; i < 31; i++ {
		path := "/settings/tokens/x/delete"
		if i%2 == 0 {
			path = "/api/v1/tokens/x"
		}
		r := httptest.NewRequest("DELETE", path, nil)
		r = r.WithContext(context.WithValue(r.Context(), ctxAuth, &authInfo{User: &model.User{ID: "a"}}))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if i == 30 && w.Code != 429 {
			t.Fatal(w.Code)
		}
	}
	if called != 30 {
		t.Fatal(called)
	}
}

func TestDuplicateCookiesAndIPv6Budget(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("Cookie", "fujipic_csrf=one; fujipic_csrf=two")
	r.Header.Set("X-CSRF-Token", "one")
	s := &Server{}
	if s.validCSRFHeader(r) || s.validCSRF(r) {
		t.Fatal("ambiguous cookie accepted")
	}
	if _, err := s.authenticate(httptest.NewRecorder(), r); err == nil {
		t.Fatal("ambiguous authentication accepted")
	}
	if rateIP("2001:db8:1:2::1") != rateIP("2001:db8:1:2::ffff") {
		t.Fatal("IPv6 rotation bypass")
	}
	if rateIP("::ffff:192.0.2.1") != "192.0.2.1" {
		t.Fatal("mapped IPv4")
	}
}

func TestMarkdownRejectsRawHTMLSyntax(t *testing.T) {
	md := markdownFor("<img src=x onerror=alert(1)> & `x` [a].png", "https://img.test/x.png")
	if strings.Contains(md, "<") || !strings.Contains(md, "&lt;img") || !strings.Contains(md, "&amp;") || !strings.Contains(md, "\\`") {
		t.Fatal(md)
	}
}

func TestMultipartCSRFPreflightIsBoundedAndReplayable(t *testing.T) {
	for _, valid := range []bool{true, false} {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		if valid {
			if err := mw.WriteField("csrf", "token"); err != nil {
				t.Fatal(err)
			}
		}
		part, err := mw.CreateFormFile("file", "test.png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(part, strings.Repeat("x", 1<<20)); err != nil {
			t.Fatal(err)
		}
		if err := mw.Close(); err != nil {
			t.Fatal(err)
		}
		original := append([]byte(nil), body.Bytes()...)
		r := httptest.NewRequest("POST", "/upload", nil)
		counted := &countedBody{Reader: bytes.NewReader(original)}
		r.Body = counted
		r.Header.Set("Content-Type", mw.FormDataContentType())
		r.AddCookie(&http.Cookie{Name: csrfCookie, Value: "token"})
		if got := (&Server{}).preflightUploadCSRF(r); got != valid {
			t.Fatal(got, valid)
		}
		if counted.read > 16<<10 {
			t.Fatal("unbounded preflight", counted.read)
		}
		if valid {
			replayed, err := io.ReadAll(r.Body)
			if err != nil || !bytes.Equal(replayed, original) {
				t.Fatal("multipart replay changed", err)
			}
		}
	}
}
