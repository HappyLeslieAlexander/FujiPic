package server_test

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

func TestDashboardHasUsableNoScriptPicker(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t, env.client, "noscript@example.com", "password-12345", "")
	resp, err := env.client.Get(env.ts.URL + "/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	picker := regexp.MustCompile(`<input[^>]*id="file-input"[^>]*>`).FindString(string(body))
	if picker == "" || strings.Contains(picker, " hidden") || strings.Contains(picker, " disabled") {
		t.Fatalf("no-JS file picker unusable: %s", picker)
	}
}

func TestImageConditionsPrecedeRange(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t, env.client, "range@example.com", "password-12345", "")
	_, img := env.upload(t, env.client, makePNG(t, 2, 2, false), "test.png", env.csrf(t, env.client), "")
	resp, err := http.Head(img.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	etag := resp.Header.Get("ETag")
	for _, r := range []string{"bytes=0-1", "bytes=999999-"} {
		for _, condition := range []string{"etag", "date"} {
			t.Run(r+condition, func(t *testing.T) {
				req, _ := http.NewRequest(http.MethodGet, img.URL, nil)
				req.Header.Set("Range", r)
				if condition == "etag" {
					req.Header.Set("If-None-Match", etag)
				} else {
					req.Header.Set("If-Modified-Since", resp.Header.Get("Last-Modified"))
				}
				got, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer got.Body.Close()
				body, _ := io.ReadAll(got.Body)
				if got.StatusCode != http.StatusNotModified || len(body) != 0 {
					t.Errorf("status=%d body=%q", got.StatusCode, body)
				}
				if v := got.Header.Get("Content-Range"); v != "" {
					t.Errorf("304 has partial Content-Range=%q", v)
				}
			})
		}
	}
}
