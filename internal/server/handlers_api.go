package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"fujipic/internal/auth"
	"fujipic/internal/id"
	"fujipic/internal/imagefile"
	"fujipic/internal/model"
	"fujipic/internal/store"
	"fujipic/internal/upload"
)

type uploadResponse struct {
	ID           string `json:"id"`
	BucketID     string `json:"bucket_id,omitempty"`
	URL          string `json:"url"`
	Markdown     string `json:"markdown"`
	HTML         string `json:"html"`
	Name         string `json:"name"`
	MIME         string `json:"mime"`
	Size         int64  `json:"size"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
	SHA256       string `json:"sha256"`
	Deduplicated bool   `json:"deduplicated"`
	CreatedAt    string `json:"created_at"`
}

func (s *Server) requireAPI(w http.ResponseWriter, r *http.Request, scope string) (*authInfo, bool) {
	info := authFrom(r)
	if info == nil || info.User == nil {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "需要登录或有效的 API Token", nil)
		return nil, false
	}
	if info.ViaBearer && scope != "" && !auth.HasScope(info.Token.Scopes, scope) {
		writeAPIError(w, http.StatusForbidden, "insufficient_scope", "Token 缺少所需权限："+scope, nil)
		return nil, false
	}
	return info, true
}

func (s *Server) requireAPICSRF(w http.ResponseWriter, r *http.Request, info *authInfo) bool {
	if info.ViaBearer {
		return true
	}
	// Cookie-authenticated writes require CSRF + Origin, matching the web
	// form handlers. Browsers send Origin on same-origin POSTs; non-browser
	// clients that omit it are still protected by the CSRF token.
	if !s.validOrigin(r) {
		writeAPIError(w, http.StatusForbidden, "origin_rejected", "跨域请求被拒绝", nil)
		return false
	}
	if !s.validCSRFHeader(r) {
		writeAPIError(w, http.StatusForbidden, "csrf_failed", "缺少或错误的 X-CSRF-Token", nil)
		return false
	}
	return true
}

func (s *Server) handleAPIUpload(w http.ResponseWriter, r *http.Request) {
	info, ok := s.requireAPI(w, r, "upload")
	if !ok {
		return
	}
	if !s.requireAPICSRF(w, r, info) {
		return
	}
	if !s.limiter.allow("upload:"+info.User.ID, 60, time.Minute) {
		tooMany(w, 60)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes+(1<<20))
	var (
		body     io.Reader
		filename string
		closeFn  func()
	)
	closeFn = func() {}
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/form-data") {
		mr, err := r.MultipartReader()
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "bad_request", "multipart 格式不正确", nil)
			return
		}
		for {
			part, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				if isRequestTooLarge(err) {
					writeAPIError(w, http.StatusRequestEntityTooLarge, "file_too_large", "图片超过大小限制", map[string]any{"max_bytes": s.cfg.MaxUploadBytes})
					return
				}
				writeAPIError(w, http.StatusBadRequest, "bad_request", "读取 multipart 失败", nil)
				return
			}
			switch part.FormName() {
			case "file":
				body = part
				filename = part.FileName()
				closeFn = func() { part.Close() }
			default:
				part.Close()
			}
			if body != nil {
				break
			}
		}
		if body == nil {
			writeAPIError(w, http.StatusBadRequest, "missing_file", "缺少 file 字段", nil)
			return
		}
	} else {
		body = r.Body
		filename = r.Header.Get("X-Filename")
	}
	defer closeFn()
	if filename == "" {
		filename = "image"
	}
	if v := r.Header.Get("X-Filename"); v != "" && !strings.HasPrefix(ct, "multipart/form-data") {
		filename = v
	}

	res, err := s.uploads.Upload(r.Context(), info.User, body, filename, "public")
	if err != nil {
		s.writeUploadError(w, err)
		return
	}
	img := res.Image
	url := s.uploads.URL(img)
	status := http.StatusCreated
	if res.Deduplicated {
		status = http.StatusOK
	}
	writeJSON(w, status, uploadResponse{
		ID:           img.ID,
		BucketID:     img.BucketID,
		URL:          url,
		Markdown:     markdownFor(img.OriginalName, url),
		HTML:         htmlFor(img.OriginalName, url),
		Name:         displayName(img.OriginalName),
		MIME:         img.MIME,
		Size:         img.SizeBytes,
		Width:        img.Width,
		Height:       img.Height,
		SHA256:       img.SHA256,
		Deduplicated: res.Deduplicated,
		CreatedAt:    time.Unix(img.CreatedAt, 0).UTC().Format(time.RFC3339),
	})
}

func (s *Server) writeUploadError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, imagefile.ErrTooLarge), isRequestTooLarge(err):
		writeAPIError(w, http.StatusRequestEntityTooLarge, "file_too_large", "图片超过大小限制", map[string]any{"max_bytes": s.cfg.MaxUploadBytes})
	case errors.Is(err, imagefile.ErrUnsupported):
		writeAPIError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "只允许 JPEG / PNG / GIF / WebP，拒绝 SVG", nil)
	case errors.Is(err, imagefile.ErrDimensions):
		writeAPIError(w, http.StatusUnprocessableEntity, "image_too_large", "图片像素或边长超过限制", nil)
	case errors.Is(err, store.ErrQuotaExceeded):
		writeAPIError(w, http.StatusForbidden, "quota_exceeded", "存储配额不足", nil)
	case errors.Is(err, upload.ErrNoCapacity):
		writeAPIError(w, http.StatusInsufficientStorage, "storage_full", "所有存储桶容量已满", nil)
	case errors.Is(err, upload.ErrOnlyPublic):
		writeAPIError(w, http.StatusBadRequest, "bad_visibility", "v1 只支持 public", nil)
	case errors.Is(err, upload.ErrStorage):
		s.log.Error("storage error", "err", err)
		writeAPIError(w, http.StatusBadGateway, "storage_error", "存储服务暂时不可用", nil)
	default:
		s.log.Error("upload failed", "err", err)
		writeAPIError(w, http.StatusInternalServerError, "internal_error", "上传失败", nil)
	}
}

// isRequestTooLarge reports whether err came from http.MaxBytesReader. The
// reader's error is wrapped by the image pipeline, so use errors.As.
func isRequestTooLarge(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

type imageItem struct {
	ID        string `json:"id"`
	BucketID  string `json:"bucket_id,omitempty"`
	URL       string `json:"url"`
	Name      string `json:"name"`
	MIME      string `json:"mime"`
	Size      int64  `json:"size"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	CreatedAt string `json:"created_at"`
}

// checkReadLimit enforces PLAN §5.6: 300 ordinary-API reads per minute,
// shared across endpoints per user so multiple tokens cannot multiply it.
func (s *Server) checkReadLimit(w http.ResponseWriter, info *authInfo) bool {
	if !s.limiter.allow("read:"+info.User.ID, 300, time.Minute) {
		tooMany(w, 60)
		return false
	}
	return true
}

func (s *Server) handleAPIListImages(w http.ResponseWriter, r *http.Request) {
	info, ok := s.requireAPI(w, r, "read")
	if !ok {
		return
	}
	if !s.checkReadLimit(w, info) {
		return
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeAPIError(w, http.StatusBadRequest, "bad_limit", "limit 需在 1 到 100 之间", nil)
			return
		}
		limit = n
	}
	var beforeCreated int64
	var beforeID string
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		var c struct {
			T  int64  `json:"t"`
			ID string `json:"id"`
		}
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(raw, &c) != nil || c.T <= 0 || c.ID == "" {
			writeAPIError(w, http.StatusBadRequest, "bad_cursor", "cursor 无效", nil)
			return
		}
		beforeCreated, beforeID = c.T, c.ID
	}
	images, err := s.store.ListImages(r.Context(), info.User.ID, limit+1, beforeCreated, beforeID)
	if err != nil {
		s.log.Error("list images", "err", err)
		writeAPIError(w, http.StatusInternalServerError, "internal_error", "读取图片列表失败", nil)
		return
	}
	nextCursor := ""
	if len(images) > limit {
		images = images[:limit]
		last := images[len(images)-1]
		payload, _ := json.Marshal(map[string]any{"t": last.CreatedAt, "id": last.ID})
		nextCursor = base64.RawURLEncoding.EncodeToString(payload)
	}
	items := make([]imageItem, 0, len(images))
	for i := range images {
		img := &images[i]
		items = append(items, imageItem{
			ID:        img.ID,
			BucketID:  img.BucketID,
			URL:       s.uploads.URL(img),
			Name:      displayName(img.OriginalName),
			MIME:      img.MIME,
			Size:      img.SizeBytes,
			Width:     img.Width,
			Height:    img.Height,
			CreatedAt: time.Unix(img.CreatedAt, 0).UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": nextCursor})
}

func (s *Server) handleAPIImage(w http.ResponseWriter, r *http.Request) {
	info, ok := s.requireAPI(w, r, "read")
	if !ok {
		return
	}
	if !s.checkReadLimit(w, info) {
		return
	}
	img, err := s.store.ImageByID(r.Context(), info.User.ID, r.PathValue("id"))
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "not_found", "图片不存在", nil)
		return
	}
	writeJSON(w, http.StatusOK, imageItem{
		ID:        img.ID,
		BucketID:  img.BucketID,
		URL:       s.uploads.URL(img),
		Name:      displayName(img.OriginalName),
		MIME:      img.MIME,
		Size:      img.SizeBytes,
		Width:     img.Width,
		Height:    img.Height,
		CreatedAt: time.Unix(img.CreatedAt, 0).UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleAPIDeleteImage(w http.ResponseWriter, r *http.Request) {
	info, ok := s.requireAPI(w, r, "delete")
	if !ok {
		return
	}
	if !s.requireAPICSRF(w, r, info) {
		return
	}
	imageID := r.PathValue("id")
	if err := s.uploads.Delete(r.Context(), info.User.ID, imageID, s.clientIP(r)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "not_found", "图片不存在", nil)
			return
		}
		s.log.Error("delete image", "err", err)
		writeAPIError(w, http.StatusInternalServerError, "internal_error", "删除失败", nil)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAPIMe(w http.ResponseWriter, r *http.Request) {
	info, ok := s.requireAPI(w, r, "read")
	if !ok {
		return
	}
	if !s.checkReadLimit(w, info) {
		return
	}
	count, _ := s.store.CountUserImages(r.Context(), info.User.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":          info.User.ID,
		"email":       info.User.Email,
		"role":        info.User.Role,
		"quota_bytes": info.User.QuotaBytes,
		"used_bytes":  info.User.UsedBytes,
		"image_count": count,
		"created_at":  time.Unix(info.User.CreatedAt, 0).UTC().Format(time.RFC3339),
	})
}

type tokenItem struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Prefix     string `json:"token_prefix"`
	Scopes     string `json:"scopes"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at,omitempty"`
	ExpiresAt  string `json:"expires_at,omitempty"`
}

func tokenItems(tokens []model.APIToken) []tokenItem {
	out := make([]tokenItem, 0, len(tokens))
	for _, t := range tokens {
		item := tokenItem{
			ID:        t.ID,
			Name:      t.Name,
			Prefix:    t.TokenPrefix,
			Scopes:    t.Scopes,
			CreatedAt: time.Unix(t.CreatedAt, 0).UTC().Format(time.RFC3339),
		}
		if t.LastUsedAt != nil {
			item.LastUsedAt = time.Unix(*t.LastUsedAt, 0).UTC().Format(time.RFC3339)
		}
		if t.ExpiresAt != nil {
			item.ExpiresAt = time.Unix(*t.ExpiresAt, 0).UTC().Format(time.RFC3339)
		}
		out = append(out, item)
	}
	return out
}

func (s *Server) handleAPITokens(w http.ResponseWriter, r *http.Request) {
	info, ok := s.requireAPI(w, r, "")
	if !ok {
		return
	}
	if info.ViaBearer {
		writeAPIError(w, http.StatusForbidden, "cookie_required", "Token 管理只能使用网页登录", nil)
		return
	}
	tokens, err := s.store.ListTokens(r.Context(), info.User.ID)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal_error", "读取 Token 失败", nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": tokenItems(tokens)})
}

func (s *Server) handleAPITokenCreate(w http.ResponseWriter, r *http.Request) {
	info, ok := s.requireAPI(w, r, "")
	if !ok {
		return
	}
	if info.ViaBearer {
		writeAPIError(w, http.StatusForbidden, "cookie_required", "Token 管理只能使用网页登录", nil)
		return
	}
	if !s.requireAPICSRF(w, r, info) {
		return
	}
	var req struct {
		Name          string `json:"name"`
		Scopes        string `json:"scopes"`
		ExpiresInDays int    `json:"expires_in_days"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "JSON 请求体不合法", nil)
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "未命名"
	}
	name = truncate(name, 80)
	scopes, ok := auth.NormalizeScopes(req.Scopes)
	if !ok {
		writeAPIError(w, http.StatusBadRequest, "bad_scopes", "scopes 只能包含 upload,read,delete", nil)
		return
	}
	var expires *int64
	if req.ExpiresInDays != 0 {
		if req.ExpiresInDays < 1 || req.ExpiresInDays > 3650 {
			writeAPIError(w, http.StatusBadRequest, "bad_expiry", "expires_in_days 需在 1 到 3650 之间", nil)
			return
		}
		v := time.Now().Add(time.Duration(req.ExpiresInDays) * 24 * time.Hour).Unix()
		expires = &v
	}
	plaintext, hash, prefix := auth.NewToken()
	tok := &model.APIToken{
		ID:          id.New("tok"),
		UserID:      info.User.ID,
		Name:        name,
		TokenHash:   hash,
		TokenPrefix: prefix,
		Scopes:      scopes,
		CreatedAt:   time.Now().Unix(),
		ExpiresAt:   expires,
	}
	if err := s.store.CreateToken(r.Context(), tok); err != nil {
		if errors.Is(err, store.ErrTokenLimitExceeded) {
			writeAPIError(w, http.StatusForbidden, "token_limit_exceeded", "Token 数量已达到上限", nil)
			return
		}
		s.log.Error("create token", "err", err)
		writeAPIError(w, http.StatusInternalServerError, "internal_error", "创建 Token 失败", nil)
		return
	}
	s.audit(r.Context(), info.User.ID, "token.create", tok.ID, s.clientIP(r), name)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":           tok.ID,
		"name":         tok.Name,
		"token":        plaintext,
		"token_prefix": tok.TokenPrefix,
		"scopes":       tok.Scopes,
		"created_at":   time.Unix(tok.CreatedAt, 0).UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleAPITokenDelete(w http.ResponseWriter, r *http.Request) {
	info, ok := s.requireAPI(w, r, "")
	if !ok {
		return
	}
	if info.ViaBearer {
		writeAPIError(w, http.StatusForbidden, "cookie_required", "Token 管理只能使用网页登录", nil)
		return
	}
	if !s.requireAPICSRF(w, r, info) {
		return
	}
	if err := s.store.DeleteToken(r.Context(), info.User.ID, r.PathValue("id")); err != nil {
		writeAPIError(w, http.StatusNotFound, "not_found", "Token 不存在", nil)
		return
	}
	s.audit(r.Context(), info.User.ID, "token.revoke", r.PathValue("id"), s.clientIP(r), "")
	w.WriteHeader(http.StatusNoContent)
}

func markdownFor(name, url string) string {
	// Escape backslashes first (simultaneous replacement) so a trailing "\"
	// cannot defuse the bracket escaping.
	alt := strings.NewReplacer("\\", "\\\\", "[", "\\[", "]", "\\]").Replace(displayName(name))
	return fmt.Sprintf("![%s](%s)", alt, url)
}

func htmlFor(name, url string) string {
	return fmt.Sprintf(`<img src="%s" alt="%s">`, html.EscapeString(url), html.EscapeString(displayName(name)))
}
