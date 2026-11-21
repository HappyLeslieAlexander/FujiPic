package server

import (
	"bytes"
	"crypto/subtle"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
)

type replayBody struct {
	io.Reader
	io.Closer
}

// The no-JS form emits CSRF first. Read at most 16 KiB in memory before
// reserving slots or allowing multipart to spill to disk, then replay that
// prefix for the normal parser. Header clients need no body preflight.
func (s *Server) preflightUploadCSRF(r *http.Request) bool {
	if len(r.CookiesNamed(csrfCookie)) != 1 {
		return false
	}
	if r.Header.Get("X-CSRF-Token") != "" {
		return s.validCSRFHeader(r)
	}
	c, err := r.Cookie(csrfCookie)
	if err != nil || c.Value == "" {
		return false
	}
	mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" {
		return false
	}
	original := r.Body
	var prefix bytes.Buffer
	reader := multipart.NewReader(io.TeeReader(io.LimitReader(original, 16<<10), &prefix), params["boundary"])
	part, err := reader.NextPart()
	if err != nil || part.FormName() != "csrf" || part.FileName() != "" {
		return false
	}
	value, err := io.ReadAll(io.LimitReader(part, 257))
	if err != nil || len(value) > 256 || subtle.ConstantTimeCompare(value, []byte(c.Value)) != 1 {
		return false
	}
	r.Body = replayBody{Reader: io.MultiReader(bytes.NewReader(prefix.Bytes()), original), Closer: original}
	return true
}
