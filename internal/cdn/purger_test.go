package cdn

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestNone(t *testing.T) {
	n := None{}
	if n.Enabled() {
		t.Fatal("none should be disabled")
	}
	if err := n.Purge(context.Background(), "x"); err == nil {
		t.Fatal("none purge should error")
	}
}
