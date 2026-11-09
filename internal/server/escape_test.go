package server

import (
	"strings"
	"testing"
)

// These helpers consume user-controlled filenames, so their outputs must be
// safe in every sink: html/template, hand-built Markdown, JSON.
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
