package server

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"fujipic/web"
)

func TestPreferredLanguage(t *testing.T) {
	for _, tc := range []struct{ header, want string }{
		{"", "en"}, {"en-US,en;q=0.9", "en"},
		{"zh-CN,zh;q=0.9,en;q=0.8", "zh-Hans"}, {"zh-SG", "zh-Hans"}, {"zh", "zh-Hans"},
		{"zh-TW", "zh-Hant"}, {"zh-HK", "zh-Hant"}, {"zh-MO", "zh-Hant"},
		{"zh-Hant", "zh-Hant"}, {"zh-Hans-TW", "zh-Hans"}, {"zh-Hant-CN", "zh-Hant"},
		{"en;q=0.5, zh-TW;q=0.9", "zh-Hant"}, {"zh-CN;q=0.5,en;q=0.9", "en"},
		{"zh-TW;q=0,zh-CN;q=0.8", "zh-Hans"}, {"zh-TW;q=0", "en"},
		{"fr-FR,zh-TW;q=0.8", "en"}, {"de-DE", "en"}, {"ja-JP", "en"},
		{"*,zh-CN;q=0.9", "en"}, {"zh-TW;q=invalid,en", "en"},
		{"zh-TW;q=NaN,en", "en"}, {"zh-TW;q=1.1,en", "en"},
		{" ZH-hk ; q=0.9 , en;q=0.8", "zh-Hant"},
		{"zh-TW;q=0.8,zh-CN;q=0.8", "zh-Hant"},
		{"<script>,zh-CN", "zh-Hans"},
		{"zh--TW, en", "en"}, {"zh-TW;q=0;q=1,en", "en"},
	} {
		t.Run(tc.header, func(t *testing.T) {
			if got := preferredLanguage(tc.header); got != tc.want {
				t.Fatalf("preferredLanguage(%q) = %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}

func TestTranslationCoverage(t *testing.T) {
	containsHan := func(s string) bool {
		return strings.ContainsFunc(s, func(r rune) bool { return unicode.Is(unicode.Han, r) })
	}
	check := func(key, source string) {
		t.Helper()
		values, ok := translations[key]
		if !ok || values[0] == "" || values[1] == "" {
			t.Errorf("missing translation for %q in %s", key, source)
		}
	}
	for key, values := range translations {
		check(key, "catalog")
		if containsHan(values[0]) {
			t.Errorf("English translation contains Chinese: %q", key)
		}
		for _, placeholder := range []string{"%s", "%d", "{count}", "{status}", "{label}"} {
			if strings.Count(key, placeholder) != strings.Count(values[0], placeholder) || strings.Count(key, placeholder) != strings.Count(values[1], placeholder) {
				t.Errorf("format placeholders differ: %q (%s)", key, placeholder)
			}
		}
	}
	for _, key := range clientMessageKeys {
		check(key, "JavaScript messages")
	}
	// Guard against forgotten visible template text and missing catalog entries.
	files, _ := web.FS.ReadDir("templates")
	messageRE := regexp.MustCompile(`\bT "([^"]+)"`)
	actionRE := regexp.MustCompile(`(?s){{.*?}}`)
	for _, file := range files {
		data, err := web.FS.ReadFile("templates/" + file.Name())
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range messageRE.FindAllStringSubmatch(string(data), -1) {
			check(match[1], file.Name())
		}
		if containsHan(actionRE.ReplaceAllString(string(data), "")) {
			t.Errorf("untranslated literal in template %s", file.Name())
		}
	}
	// Every Chinese server string is a message ID, including error helpers.
	paths, _ := filepath.Glob("*.go")
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") || path == "i18n.go" {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				value, _ := strconv.Unquote(lit.Value)
				if containsHan(value) {
					check(value, path)
				}
			}
			return true
		})
	}
	// JSON maps must not silently overwrite duplicate message IDs.
	raw, err := os.ReadFile("translations.json")
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	_, _ = decoder.Token()
	seen := map[string]bool{}
	for decoder.More() {
		key, _ := decoder.Token()
		name := key.(string)
		if seen[name] {
			t.Errorf("duplicate translation: %s", name)
		}
		seen[name] = true
		var values [2]string
		if err := decoder.Decode(&values); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBucketMessageTranslation(t *testing.T) {
	for _, lang := range []string{"en", "zh-Hant"} {
		got := translateBucketMessage(lang, "OCI 端点区域为 ap-tokyo-1，但填写的区域为 us-east-1；请使用目标桶所在区域的端点和区域")
		if !strings.Contains(got, "ap-tokyo-1") || !strings.Contains(got, "us-east-1") || strings.Contains(got, "填写") {
			t.Fatal(got)
		}
		got = translateBucketMessage(lang, "无法解析端点主机: example.com: lookup failed")
		if !strings.HasSuffix(got, ": example.com: lookup failed") || strings.Contains(got, "端点") {
			t.Fatal(got)
		}
	}
}
