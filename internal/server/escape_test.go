package server

import (
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
		"cat.png":     "cat.png",
		"a\nb\rc.png": "abc.png",
		"a\tb.png":    "ab.png",
		"图片.png":      "图片.png",
		"":            "image",
		"   ":         "image",
		"\x00\x1f":    "image",
		"a\x7fb.png":  "ab.png",
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
