package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

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
