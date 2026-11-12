package server

import (
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"fujipic/internal/storage"
)

// TestNotModifiedWildcardWithoutETag pins RFC 9110 §13.1.2: If-None-Match: *
// matches any current representation, including one whose storage backend did
// not expose an ETag.
func TestNotModifiedWildcardWithoutETag(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/i/2026/11/abcdefghijklmnopqrstuvwxyz.png", nil)
	req.Header.Set("If-None-Match", "*")
	if !notModified(req, &storage.Object{}) {
		t.Fatal("If-None-Match: * did not match a representation without an ETag")
	}

	req.Header.Set("If-None-Match", `"other"`)
	if notModified(req, &storage.Object{}) {
		t.Fatal("a non-matching entity tag matched a representation without an ETag")
	}
}

func TestIfRangeMatches(t *testing.T) {
	modified := time.Date(2026, 11, 10, 12, 34, 56, 0, time.UTC)
	obj := &storage.Object{ETag: `"abc123"`, LastModified: modified}
	cases := []struct {
		name      string
		validator string
		want      bool
	}{
		{"strong etag", `"abc123"`, true},
		{"different etag", `"other"`, false},
		{"weak etag", `W/"abc123"`, false},
		{"date equal", modified.Format(http.TimeFormat), true},
		{"date earlier", modified.Add(-time.Hour).Format(http.TimeFormat), false},
		{"malformed", "not-a-validator", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		if got := ifRangeMatches(c.validator, obj); got != c.want {
			t.Errorf("%s: ifRangeMatches(%q) = %v, want %v", c.name, c.validator, got, c.want)
		}
	}

	// A weak current ETag can never satisfy a strong If-Range comparison.
	if ifRangeMatches(`"abc123"`, &storage.Object{ETag: `W/"abc123"`}) {
		t.Fatal("weak current ETag matched a strong validator")
	}
}

func TestMaxRequestBytesSaturates(t *testing.T) {
	if got := maxRequestBytes(math.MaxInt64); got != math.MaxInt64 {
		t.Fatalf("maxRequestBytes(MaxInt64) = %d, want MaxInt64", got)
	}
	const limit = int64(10)
	if got := maxRequestBytes(limit); got != limit+(1<<20) {
		t.Fatalf("maxRequestBytes(%d) = %d, want %d", limit, got, limit+(1<<20))
	}
}
