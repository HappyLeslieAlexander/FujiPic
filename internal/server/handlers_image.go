package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"fujipic/internal/bucket"
	"fujipic/internal/storage"
	"fujipic/internal/store"
)

var objectKeyPattern = regexp.MustCompile(`^i/\d{4}/\d{2}/[a-z2-7]{26}\.(jpg|png|gif|webp)$`)

func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	key := "i/" + r.PathValue("key")
	if !objectKeyPattern.MatchString(key) {
		imageNotFound(w)
		return
	}
	img, err := s.store.ImageByObjectKey(r.Context(), key)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			imageNotFound(w)
			return
		}
		s.log.Error("look up image", "key", key, "err", err)
		imageError(w, http.StatusInternalServerError)
		return
	}

	const cacheControl = "public, max-age=31536000, immutable"
	h := w.Header()
	h.Set("Cache-Control", cacheControl)
	h.Set("CDN-Cache-Control", "public, max-age=31536000")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Disposition", "inline")

	objects, err := s.buckets.Resolve(r.Context(), img.BucketID)
	if err != nil {
		if errors.Is(err, bucket.ErrNotFound) || errors.Is(err, bucket.ErrNoEncryptionKey) {
			imageNotFound(w)
			return
		}
		s.log.Error("resolve image bucket", "bucket", img.BucketID, "key", key, "err", err)
		imageError(w, http.StatusInternalServerError)
		return
	}

	if r.Method == http.MethodHead {
		obj, err := objects.Head(r.Context(), key)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				imageNotFound(w)
				return
			}
			s.log.Error("head image object", "key", key, "err", err)
			imageError(w, http.StatusBadGateway)
			return
		}
		writeObjectHeaders(h, obj, img.MIME, img.SizeBytes)
		if notModified(r, obj) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	// RFC 9110 §13.1.5: If-Range is a precondition for using the Range
	// header. When its validator does not match the current representation,
	// the server must ignore Range and return the full object. The object
	// key is immutable, so a HEAD is only needed for requests that actually
	// carry both headers.
	rangeHeader := r.Header.Get("Range")
	if rangeHeader != "" && r.Header.Get("If-Range") != "" {
		obj, err := objects.Head(r.Context(), key)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				imageNotFound(w)
				return
			}
			s.log.Error("head image for if-range", "key", key, "err", err)
			imageError(w, http.StatusBadGateway)
			return
		}
		if !ifRangeMatches(r.Header.Get("If-Range"), obj) {
			rangeHeader = ""
		}
	}

	obj, err := objects.Get(r.Context(), key, rangeHeader)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			imageNotFound(w)
			return
		}
		s.log.Error("read image object", "key", key, "err", err)
		imageError(w, http.StatusBadGateway)
		return
	}
	defer obj.Body.Close()

	writeObjectHeaders(h, obj, img.MIME, img.SizeBytes)
	if obj.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		h.Set("Cache-Control", "no-store")
		h.Del("CDN-Cache-Control")
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if notModified(r, obj) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	status := obj.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = io.Copy(w, obj.Body)
}

func writeObjectHeaders(h http.Header, obj *storage.Object, fallbackMIME string, fallbackSize int64) {
	mimeType := obj.ContentType
	if mimeType == "" {
		mimeType = fallbackMIME
	}
	h.Set("Content-Type", mimeType)
	if obj.ETag != "" {
		h.Set("ETag", obj.ETag)
	}
	if !obj.LastModified.IsZero() {
		h.Set("Last-Modified", obj.LastModified.UTC().Format(http.TimeFormat))
	}
	if obj.ContentRange != "" {
		h.Set("Content-Range", obj.ContentRange)
	} else if obj.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		// RFC 9110 §15.5.17 requires an unsatisfied-range specifier on 416.
		// S3 returns InvalidRange without the object size, so use the size
		// recorded at upload time.
		h.Set("Content-Range", fmt.Sprintf("bytes */%d", fallbackSize))
	}
	if obj.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		h.Set("Content-Length", strconv.FormatInt(obj.Size, 10))
	}
}

func notModified(r *http.Request, obj *storage.Object) bool {
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		// RFC 9110 §13.1.2: "*" matches any current representation, even
		// when the storage backend did not expose an entity tag.
		for _, candidate := range strings.Split(inm, ",") {
			if strings.TrimSpace(candidate) == "*" {
				return true
			}
		}
		// RFC 9110 §13.1.3: If-None-Match takes precedence over
		// If-Modified-Since. When the header is present but no current
		// entity tag matches, the response must not be a 304 just because
		// the date also happens to be old.
		if obj.ETag == "" {
			return false
		}
		for _, candidate := range strings.Split(inm, ",") {
			candidate = strings.TrimSpace(candidate)
			if candidate == obj.ETag || strings.TrimPrefix(candidate, "W/") == strings.TrimPrefix(obj.ETag, "W/") {
				return true
			}
		}
		return false
	}
	if ims := r.Header.Get("If-Modified-Since"); ims != "" && !obj.LastModified.IsZero() {
		if t, err := http.ParseTime(ims); err == nil {
			if !obj.LastModified.Truncate(time.Second).After(t) {
				return true
			}
		}
	}
	return false
}

func imageNotFound(w http.ResponseWriter) {
	imageError(w, http.StatusNotFound)
}

func imageError(w http.ResponseWriter, status int) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Del("CDN-Cache-Control")
	http.Error(w, "image unavailable", status)
}

// ifRangeMatches implements RFC 9110 §13.1.5. Entity-tag validators require
// a strong comparison, so weak tags never match. A date validator matches
// only when it is exactly the representation's Last-Modified time.
func ifRangeMatches(validator string, obj *storage.Object) bool {
	v := strings.TrimSpace(validator)
	if v == "" {
		return false
	}
	if strings.HasPrefix(v, `"`) || strings.HasPrefix(v, "W/") {
		if strings.HasPrefix(v, "W/") || strings.HasPrefix(obj.ETag, "W/") || obj.ETag == "" {
			return false
		}
		return v == obj.ETag
	}
	if obj.LastModified.IsZero() {
		return false
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return false
	}
	return obj.LastModified.Unix() == t.Unix()
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	dbOK := s.store.Ping(ctx) == nil
	storageOK := s.buckets.Default().Health(ctx) == nil
	status := http.StatusOK
	if !dbOK || !storageOK {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, map[string]any{
		"status":  map[bool]string{true: "ok", false: "degraded"}[status == http.StatusOK],
		"db":      dbOK,
		"storage": storageOK,
	})
}
