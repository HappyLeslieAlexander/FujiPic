package server

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"fujipic/internal/config"
)

// These helpers consume user-controlled filenames, so their outputs must be
// safe in every sink: html/template, hand-built Markdown, JSON.
func TestLimiterCleansExpiredEntriesPeriodically(t *testing.T) {
	l := newLimiter()
	l.entries["expired"] = &limitEntry{reset: time.Now().Add(-time.Minute), count: 1}
	if !l.allow("fresh", 1, time.Minute) {
		t.Fatal("fresh key should be allowed")
	}
	if _, ok := l.entries["expired"]; ok {
		t.Fatal("expired entry was not cleaned")
	}
	if l.nextCleanup.IsZero() {
		t.Fatal("periodic cleanup schedule was not initialized")
	}
}

func TestClientIPTrustedProxy(t *testing.T) {
	s := &Server{cfg: &config.Config{
		TrustProxy:        true,
		TrustedProxyCIDRs: []string{"127.0.0.0/8"},
		ClientIPHeader:    "CF-Connecting-IP",
	}}

	direct := httptest.NewRequest(http.MethodGet, "/", nil)
	direct.RemoteAddr = "203.0.113.10:1234"
	direct.Header.Set("CF-Connecting-IP", "198.51.100.20")
	if got := s.clientIP(direct); got != "203.0.113.10" {
		t.Fatalf("untrusted direct IP = %q, want forged header ignored", got)
	}

	trusted := httptest.NewRequest(http.MethodGet, "/", nil)
	trusted.RemoteAddr = "127.0.0.1:1234"
	trusted.Header.Set("CF-Connecting-IP", "198.51.100.20")
	if got := s.clientIP(trusted); got != "198.51.100.20" {
		t.Fatalf("trusted proxy IP = %q, want forwarded client IP", got)
	}
}

func TestTruncatePreservesUTF8(t *testing.T) {
	got := truncate(strings.Repeat("汉字", 100), 80)
	if utf8.RuneCountInString(got) != 80 || !utf8.ValidString(got) {
		t.Fatalf("truncated Unicode output is damaged: count=%d valid=%v", utf8.RuneCountInString(got), utf8.ValidString(got))
	}
}

func TestDisplayNameStripsControlChars(t *testing.T) {
	cases := map[string]string{
		"cat.png":      "cat.png",
		"a\nb\rc.png":  "abc.png",
		"a\tb.png":     "ab.png",
		"图片.png":       "图片.png",
		"":             "image",
		"   ":          "image",
		"\x00\x1f":     "image",
		"a\x7fb.png":   "ab.png",
		"a\u0085b.png": "ab.png", // C1 control
		"a\u202Eb.png": "ab.png", // right-to-left override
		"a\u200Bb.png": "ab.png", // zero-width space
		"a\u2028b.png": "ab.png", // line separator
		"a\u2066b.png": "ab.png", // left-to-right isolate
	}
	for in, want := range cases {
		if got := displayName(in); got != want {
			t.Errorf("displayName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMarkdownForEscapes(t *testing.T) {
	url := "https://img.example.com/i/x.png"
	md := markdownFor("a]b[c.png", url)
	if !strings.Contains(md, `a\]b\[c.png`) {
		t.Errorf("brackets not escaped: %s", md)
	}
	md = markdownFor("a\\]b.png", url)
	if !strings.Contains(md, `a\\\]b.png`) {
		t.Errorf("backslash+bracket not escaped: %s", md)
	}
	md = markdownFor("a\nb.png", url)
	if strings.ContainsAny(md, "\r\n") {
		t.Errorf("newline survives in markdown: %q", md)
	}
	if md != "![ab.png]("+url+")" {
		t.Errorf("markdown = %q", md)
	}
}

func TestHTMLForEscapes(t *testing.T) {
	url := "https://img.example.com/i/x.png"
	html := htmlFor(`<script>alert("x")</script>.png`, url)
	if strings.Contains(html, "<script>") {
		t.Errorf("raw script tag survives: %s", html)
	}
	for _, want := range []string{"&lt;script&gt;", "&#34;", "&lt;/script&gt;"} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q in %s", want, html)
		}
	}
	if strings.ContainsAny(html, "\r\n") {
		t.Errorf("control char in html embed: %q", html)
	}
}

func TestValidOriginNormalizesDefaultPorts(t *testing.T) {
	cases := []struct {
		base   string
		origin string
		want   bool
	}{
		{"https://example.com", "https://example.com", true},
		{"https://example.com:443", "https://example.com", true},
		{"https://example.com", "https://example.com:443", true},
		{"http://example.com:80", "http://example.com", true},
		{"https://example.com:8443", "https://example.com:8443", true},
		{"https://example.com", "https://example.com:8443", false},
		{"https://example.com", "http://example.com", false},
		{"https://example.com", "https://evil.example.com", false},
		{"https://example.com", "null", false},
		{"https://example.com", "", true}, // non-browser client without Origin
		{"https://example.com", "https://EXAMPLE.com", true},
	}
	for _, c := range cases {
		s := &Server{cfg: &config.Config{AppBaseURL: c.base}}
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if got := s.validOrigin(r); got != c.want {
			t.Errorf("base=%q origin=%q: validOrigin = %v, want %v", c.base, c.origin, got, c.want)
		}
	}
}

func TestStatusWriterKeepsFirstStatus(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &statusWriter{ResponseWriter: rec, status: http.StatusOK}
	w.WriteHeader(http.StatusCreated)
	w.WriteHeader(http.StatusInternalServerError)
	if w.status != http.StatusCreated {
		t.Fatalf("logged status = %d, want first status 201", w.status)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("response status = %d, want 201", rec.Code)
	}

	// A handler that writes without an explicit WriteHeader reports 200.
	rec = httptest.NewRecorder()
	w = &statusWriter{ResponseWriter: rec, status: http.StatusOK}
	if _, err := w.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	if w.status != http.StatusOK || rec.Code != http.StatusOK {
		t.Fatalf("implicit status = %d/%d, want 200/200", w.status, rec.Code)
	}
}

func TestWriteUploadErrorMapsMaxBytesError(t *testing.T) {
	s := &Server{
		cfg: &config.Config{MaxUploadBytes: 123},
		log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	rec := httptest.NewRecorder()
	s.writeUploadError(rec, fmt.Errorf("write temp file: %w", &http.MaxBytesError{Limit: 123}))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "file_too_large") {
		t.Fatalf("body = %s, want file_too_large", body)
	}
}
