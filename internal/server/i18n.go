package server

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// Chinese source strings are stable message IDs. Catalog values are plain text,
// never trusted HTML; html/template performs escaping at the point of use.
//
//go:embed translations.json
var translationFS embed.FS

var translations = loadTranslations()

var languageRange = regexp.MustCompile(`^(\*|[a-z]{1,8}(-[a-z0-9]{1,8})*)$`)

func loadTranslations() map[string][2]string {
	data, err := translationFS.ReadFile("translations.json")
	if err != nil {
		panic(err)
	}
	var catalog map[string][2]string
	if err := json.Unmarshal(data, &catalog); err != nil {
		panic(err)
	}
	return catalog
}

// preferredLanguage selects the highest-weight valid browser preference, with
// stable ordering for ties. Unsupported preferences fall back to English rather
// than skipping ahead to a less-preferred language. q=0 is never selected.
func preferredLanguage(header string) string {
	best, weight := "en", -1.0
	for _, entry := range strings.Split(header, ",") {
		parts := strings.Split(strings.TrimSpace(entry), ";")
		tag := strings.ToLower(strings.TrimSpace(parts[0]))
		if tag == "" {
			continue
		}
		valid := languageRange.MatchString(tag) && len(parts) <= 2
		q := 1.0
		for _, param := range parts[1:] {
			key, value, ok := strings.Cut(strings.TrimSpace(param), "=")
			if !ok || !strings.EqualFold(key, "q") {
				valid = false
				break
			}
			var err error
			q, err = strconv.ParseFloat(value, 64)
			if err != nil || !(q >= 0 && q <= 1) {
				valid = false
			}
		}
		if !valid || q <= 0 || q <= weight {
			continue
		}
		weight = q
		best = "en"
		subtags := strings.Split(tag, "-")
		if subtags[0] != "zh" {
			continue
		}
		best = "zh-Hans"
		// An explicit script wins over region (e.g. zh-Hans-TW).
		for _, subtag := range subtags[1:] {
			if subtag == "hant" || subtag == "hans" {
				if subtag == "hant" {
					best = "zh-Hant"
				}
				goto selected
			}
		}
		for _, subtag := range subtags[1:] {
			if subtag == "tw" || subtag == "hk" || subtag == "mo" {
				best = "zh-Hant"
			}
		}
	selected:
	}
	return best
}

func translate(lang, message string) string {
	if lang == "zh-Hans" || message == "" {
		return message
	}
	if values, ok := translations[message]; ok {
		if lang == "zh-Hant" {
			return values[1]
		}
		return values[0]
	}
	return message
}

// Registry validation errors can carry endpoint/SDK details. Translate only
// known diagnostic text, never arbitrary user input or provider error content.
var ociRegionMessage = regexp.MustCompile(`^OCI 端点区域为 ([a-z0-9-]+)，但填写的区域为 ([a-z0-9-]+)；请使用目标桶所在区域的端点和区域$`)

func translateBucketMessage(lang, message string) string {
	if match := ociRegionMessage.FindStringSubmatch(message); match != nil {
		return fmt.Sprintf(translate(lang, "OCI 端点区域为 %s，但填写的区域为 %s；请使用目标桶所在区域的端点和区域"), match[1], match[2])
	}
	for _, prefix := range []string{"无法创建存储客户端", "无法读写存储桶，请检查端点、区域和凭据", "无法解析端点主机"} {
		if details, ok := strings.CutPrefix(message, prefix+": "); ok {
			return translate(lang, prefix) + ": " + details
		}
	}
	return translate(lang, message)
}

func responseLanguage(w http.ResponseWriter) string {
	if lang := w.Header().Get("Content-Language"); lang != "" {
		return lang
	}
	return "en"
}

func localizedError(w http.ResponseWriter, message string, status int) {
	http.Error(w, translate(responseLanguage(w), message), status)
}

func (s *Server) withLanguage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Images, static assets and probes are language-neutral. Do not fragment
		// the CDN cache or change their streaming/cookie-free behavior.
		if !skipAuth(r.URL.Path) {
			w.Header().Set("Content-Language", preferredLanguage(r.Header.Get("Accept-Language")))
			w.Header().Add("Vary", "Accept-Language")
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (p pageData) T(message string) string { return translate(p.Language, message) }

func (p pageData) TurnstileLanguage() string {
	switch p.Language {
	case "zh-Hans":
		return "zh-cn"
	case "zh-Hant":
		return "zh-tw"
	default:
		return "en"
	}
}

var clientMessageKeys = []string{
	"等待中", "刚刚", "链接", "中国大陆优化链接", "中国大陆优化 Markdown", "删除",
	"已存在，返回原链接", "上传成功", "失败（HTTP {status}）", "网络错误",
	"只支持图片文件（JPEG / PNG / GIF / WebP）。", "请先选择图片。", "{label}，已复制",
	"删除这张图片？CDN 缓存刷新可能需要一点时间。", "还没有图片，上传第一张吧。",
	"删除失败（HTTP {status}）", "网络错误，删除失败。",
	"已复制", "已选 {count} 张", "确定删除全部 {count} 张筛选结果（包含其他分页）？此操作不可撤销。",
	"确定删除选中的 {count} 张图片？此操作不可撤销。",
}

func clientMessages(lang string) map[string]string {
	messages := make(map[string]string, len(clientMessageKeys))
	for _, key := range clientMessageKeys {
		messages[key] = translate(lang, key)
	}
	return messages
}
