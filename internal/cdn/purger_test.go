package cdn

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestGenericPurge(t *testing.T) {
	var gotAuth, gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	g := &Generic{Endpoint: ts.URL, Token: "secret"}
	if !g.Enabled() || g.Name() != "generic" {
		t.Fatalf("generic = %+v", g)
	}
	if err := g.Purge(context.Background(), "https://img.example.com/i/a.png"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("auth = %q", gotAuth)
	}
	var payload struct {
		URLs []string `json:"urls"`
	}
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil || len(payload.URLs) != 1 {
		t.Fatalf("payload = %q", gotBody)
	}
}

func TestGenericPurgeBatch(t *testing.T) {
	var gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	g := &Generic{Endpoint: ts.URL, Token: "secret"}
	if err := g.PurgeBatch(context.Background(), []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		URLs []string `json:"urls"`
	}
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil || len(payload.URLs) != 2 {
		t.Fatalf("payload = %q", gotBody)
	}
}

func TestGenericPurgeError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer ts.Close()
	g := &Generic{Endpoint: ts.URL, Token: "bad"}
	err := g.Purge(context.Background(), "https://img.example.com/i/a.png")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
}

func TestCloudflarePurge(t *testing.T) {
	var gotPath, gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"success":true,"errors":[]}`))
	}))
	defer ts.Close()
	c := &Cloudflare{ZoneID: "zone123", Token: "tok", Endpoint: ts.URL + "/zones/zone123/purge_cache", Client: ts.Client()}
	if err := c.Purge(context.Background(), "https://img.example.com/i/a.png"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/zones/zone123/purge_cache" {
		t.Fatalf("path = %q", gotPath)
	}
	if !strings.Contains(gotBody, `"files"`) {
		t.Fatalf("body = %q", gotBody)
	}
}

func TestCloudflarePurgeBatch(t *testing.T) {
	var gotBody string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"success":true,"errors":[]}`))
	}))
	defer ts.Close()
	c := &Cloudflare{ZoneID: "zone123", Token: "tok", Endpoint: ts.URL + "/zones/zone123/purge_cache", Client: ts.Client()}
	if err := c.PurgeBatch(context.Background(), []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Files []string `json:"files"`
	}
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil || len(payload.Files) != 2 {
		t.Fatalf("payload = %q", gotBody)
	}
}

func TestCloudflarePurgeRequiresConfirmedSuccess(t *testing.T) {
	for _, tc := range []struct {
		name      string
		body      string
		truncated bool
		wantError bool
	}{
		{name: "confirmed", body: `{"success":true,"errors":[]}`},
		{name: "rejected", body: `{"success":false}`, wantError: true},
		{name: "empty", wantError: true},
		{name: "html", body: `<html>upstream unavailable</html>`, wantError: true},
		{name: "broken JSON", body: `{"success":`, wantError: true},
		{name: "missing confirmation", body: `{}`, wantError: true},
		{name: "null confirmation", body: `{"success":null}`, wantError: true},
		{name: "wrong confirmation type", body: `{"success":"true"}`, wantError: true},
		{name: "trailing garbage", body: `{"success":true}broken`, wantError: true},
		{name: "oversized", body: `{"success":true}` + strings.Repeat(" ", 64<<10), wantError: true},
		{name: "truncated HTTP body", body: `{"success":true}`, truncated: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.truncated {
					// A complete JSON value does not make an incomplete HTTP
					// response trustworthy: reading the body must succeed too.
					w.Header().Set("Content-Length", strconv.Itoa(len(tc.body)+10))
				}
				_, _ = io.WriteString(w, tc.body)
			}))
			defer ts.Close()
			c := &Cloudflare{ZoneID: "zone", Token: "token", Endpoint: ts.URL, Client: ts.Client()}
			err := c.Purge(context.Background(), "https://img.example.com/i/a.png")
			if (err != nil) != tc.wantError {
				t.Fatalf("Purge error = %v, wantError = %v", err, tc.wantError)
			}
		})
	}
}

func TestNone(t *testing.T) {
	n := None{}
	if n.Enabled() {
		t.Fatal("none should be disabled")
	}
	if err := n.Purge(context.Background(), "x"); err == nil {
		t.Fatal("none purge should error")
	}
}

func TestGenericPurgeRejectsIncompleteResponse(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if oversized {
				_, _ = io.WriteString(w, strings.Repeat("x", (64<<10)+1))
			} else {
				w.Header().Set("Content-Length", "100")
				_, _ = io.WriteString(w, "partial")
			}
		}))
		g := &Generic{Endpoint: ts.URL, Token: "test-token", Client: ts.Client()}
		err := g.Purge(context.Background(), "https://img.example.test/i/a.png")
		ts.Close()
		if err == nil {
			t.Fatalf("incomplete response acknowledged (oversized=%v)", oversized)
		}
	}
}
