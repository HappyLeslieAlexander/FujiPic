// Package server wires the HTTP API, the web UI and the CDN origin endpoint.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"fujipic/internal/bucket"
	"fujipic/internal/cdn"
	"fujipic/internal/config"
	"fujipic/internal/id"
	"fujipic/internal/model"
	"fujipic/internal/store"
	"fujipic/internal/upload"
	"fujipic/web"
)

const (
	sessionCookie = "fujipic_session"
	csrfCookie    = "fujipic_csrf"
	flashCookie   = "fujipic_flash"
)

type Server struct {
	cfg     *config.Config
	store   *store.Store
	buckets *bucket.Registry
	uploads *upload.Service
	purger  cdn.Purger
	log     *slog.Logger
	tmpl    *template.Template
	limiter *limiter
	started time.Time
}

func New(cfg *config.Config, st *store.Store, buckets *bucket.Registry, up *upload.Service, purger cdn.Purger, log *slog.Logger) (*Server, error) {
	funcs := template.FuncMap{
		"formatBytes": formatBytes,
		"formatTime":  formatTime,
		"formatTimePtr": func(v *int64) string {
			if v == nil {
				return ""
			}
			return formatTime(*v)
		},
		"quotaMB": func(n int64) int64 { return n / (1 << 20) },
		"maxGB":   func(n int64) float64 { return float64(n) / float64(int64(1)<<30) },
		"maskKey": func(s string) string {
			if len(s) <= 4 {
				return "…"
			}
			return s[:4] + "…"
		},
		"pct": func(used, quota int64) int {
			if quota <= 0 {
				return 0
			}
			p := int(used * 100 / quota)
			if p > 100 {
				p = 100
			}
			return p
		},
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFS(web.FS, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return &Server{
		cfg:     cfg,
		store:   st,
		buckets: buckets,
		uploads: up,
		purger:  purger,
		log:     log,
		tmpl:    tmpl,
		limiter: newLimiter(),
		started: time.Now(),
	}, nil
}

func (s *Server) Handler() http.Handler {
	mux := s.routes()
	var h http.Handler = mux
	h = s.withAccessLog(h)
	h = s.withAuth(h)
	h = s.withOriginSecret(h)
	h = s.withSecurityHeaders(h)
	h = s.withRequestID(h)
	h = s.withRecovery(h)
	return h
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)

	staticFS, _ := fs.Sub(web.FS, "static")
	staticHandler := http.FileServer(http.FS(staticFS))
	mux.Handle("GET /static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// CDN.md 把静态资源规则定为 7 天；源站显式声明，避免各 CDN 默认值不一致。
		w.Header().Set("Cache-Control", "public, max-age=604800")
		http.StripPrefix("/static/", staticHandler).ServeHTTP(w, r)
	}))

	mux.HandleFunc("GET /i/{key...}", s.handleImage)

	mux.HandleFunc("GET /", s.handleRoot)
	mux.HandleFunc("GET /login", s.handleLoginPage)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("GET /register", s.handleRegisterPage)
	mux.HandleFunc("POST /register", s.handleRegister)
	mux.HandleFunc("POST /logout", s.handleLogout)
	mux.HandleFunc("GET /dashboard", s.handleDashboard)
	mux.HandleFunc("POST /upload", s.handleWebUpload)
	mux.HandleFunc("GET /settings", s.handleSettings)
	mux.HandleFunc("POST /settings/password", s.handlePasswordChange)
	mux.HandleFunc("POST /settings/tokens", s.handleTokenCreateWeb)
	mux.HandleFunc("POST /settings/tokens/{id}/delete", s.handleTokenDeleteWeb)
	mux.HandleFunc("GET /admin", s.handleAdmin)
	mux.HandleFunc("POST /admin/buckets", s.handleAdminBucketCreate)
	mux.HandleFunc("POST /admin/buckets/{id}", s.handleAdminBucketUpdate)
	mux.HandleFunc("POST /admin/buckets/{id}/delete", s.handleAdminBucketDelete)
	mux.HandleFunc("POST /admin/users/{id}/status", s.handleAdminUserStatus)
	mux.HandleFunc("POST /admin/users/{id}/quota", s.handleAdminUserQuota)
	mux.HandleFunc("POST /admin/invites", s.handleAdminInvite)

	mux.HandleFunc("POST /api/v1/upload", s.handleAPIUpload)
	mux.HandleFunc("GET /api/v1/images", s.handleAPIListImages)
	mux.HandleFunc("GET /api/v1/images/{id}", s.handleAPIImage)
	mux.HandleFunc("DELETE /api/v1/images/{id}", s.handleAPIDeleteImage)
	mux.HandleFunc("GET /api/v1/me", s.handleAPIMe)
	mux.HandleFunc("GET /api/v1/tokens", s.handleAPITokens)
	mux.HandleFunc("POST /api/v1/tokens", s.handleAPITokenCreate)
	mux.HandleFunc("DELETE /api/v1/tokens/{id}", s.handleAPITokenDelete)

	return mux
}

// ---- context / auth ----

type ctxKey int

const (
	ctxAuth ctxKey = iota
	ctxRequestID
)

type authInfo struct {
	User      *model.User
	SessionID string
	Token     *model.APIToken
	ViaBearer bool
}

func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if skipAuth(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		info, err := s.authenticate(r)
		if err == nil && info != nil {
			r = r.WithContext(context.WithValue(r.Context(), ctxAuth, info))
		}
		next.ServeHTTP(w, r)
	})
}

func skipAuth(path string) bool {
	return strings.HasPrefix(path, "/static/") ||
		strings.HasPrefix(path, "/i/") ||
		path == "/healthz" || path == "/readyz"
}

func (s *Server) authenticate(r *http.Request) (*authInfo, error) {
	if h := r.Header.Get("Authorization"); h != "" {
		if !strings.HasPrefix(h, "Bearer ") {
			return nil, errors.New("unsupported authorization scheme")
		}
		raw := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		if raw == "" {
			return nil, errors.New("empty bearer token")
		}
		user, token, err := s.store.TokenUser(r.Context(), id.Hash(raw))
		if err != nil {
			return nil, err
		}
		s.store.TouchToken(r.Context(), token.ID)
		return &authInfo{User: user, Token: token, ViaBearer: true}, nil
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil, nil
	}
	user, sessionID, err := s.store.SessionUser(r.Context(), id.Hash(c.Value))
	if err != nil {
		return nil, err
	}
	s.store.TouchSession(r.Context(), sessionID)
	return &authInfo{User: user, SessionID: sessionID}, nil
}

func authFrom(r *http.Request) *authInfo {
	info, _ := r.Context().Value(ctxAuth).(*authInfo)
	return info
}

func userFrom(r *http.Request) *model.User {
	if info := authFrom(r); info != nil {
		return info.User
	}
	return nil
}

// webUserFrom is the web-console counterpart of userFrom. API Bearer
// credentials are machine credentials; they must not be treated as an
// interactive browser session for dashboard, settings or admin pages.
func webUserFrom(r *http.Request) *model.User {
	if info := authFrom(r); info != nil && !info.ViaBearer {
		return info.User
	}
	return nil
}

// requireWebUser gives web handlers a cookie-session-only authenticated
// user. Bearer tokens receive a direct 403 so the distinction is visible to
// non-browser callers and cannot be confused with missing login state.
func (s *Server) requireWebUser(w http.ResponseWriter, r *http.Request) *model.User {
	if info := authFrom(r); info != nil && info.ViaBearer {
		http.Error(w, "API tokens cannot access the web console", http.StatusForbidden)
		return nil
	}
	user := webUserFrom(r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return nil
	}
	return user
}

func (s *Server) createSession(ctx context.Context, w http.ResponseWriter, r *http.Request, userID string) error {
	raw := id.Random(32)
	now := time.Now()
	sess := &model.Session{
		ID:         id.New("ses"),
		UserID:     userID,
		TokenHash:  id.Hash(raw),
		IP:         s.clientIP(r),
		UserAgent:  truncate(r.UserAgent(), 300),
		CreatedAt:  now.Unix(),
		LastSeenAt: now.Unix(),
		ExpiresAt:  now.Add(s.cfg.SessionTTL).Unix(),
	}
	if err := s.store.CreateSession(ctx, sess); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    raw,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(s.cfg.SessionTTL.Seconds()),
	})
	// Entering the authenticated state is a privilege boundary: rotate CSRF
	// even if a client-supplied cookie already exists.
	s.rotateCSRF(w, r)
	return nil
}

func (s *Server) destroySession(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	if info := authFrom(r); info != nil && info.SessionID != "" {
		_ = s.store.DeleteSession(ctx, info.SessionID)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/", HttpOnly: true,
		Secure: s.cfg.SecureCookies(), SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
	// The CSRF cookie is also retired at logout so a fixed value cannot live
	// on with the browser session.
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookie, Value: "", Path: "/", HttpOnly: true,
		Secure: s.cfg.SecureCookies(), SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

// ---- CSRF ----

func (s *Server) ensureCSRF(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(csrfCookie); err == nil && len(c.Value) >= 20 {
		return c.Value
	}
	return s.rotateCSRF(w, r)
}

func (s *Server) rotateCSRF(w http.ResponseWriter, r *http.Request) string {
	// Privilege transitions always replace the CSRF value, rather than
	// reusing a potentially fixed client-supplied cookie.
	token := id.Random(32)
	http.SetCookie(w, &http.Cookie{
		Name:     csrfCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   30 * 24 * 3600,
	})
	return token
}

func (s *Server) validCSRFHeader(r *http.Request) bool {
	c, err := r.Cookie(csrfCookie)
	if err != nil || c.Value == "" {
		return false
	}
	supplied := r.Header.Get("X-CSRF-Token")
	if supplied == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(supplied)) == 1
}

func (s *Server) validCSRF(r *http.Request) bool {
	if !s.validOrigin(r) {
		return false
	}
	if s.validCSRFHeader(r) {
		return true
	}
	c, err := r.Cookie(csrfCookie)
	if err != nil || c.Value == "" {
		return false
	}
	if err := r.ParseForm(); err != nil {
		return false
	}
	supplied := r.FormValue("csrf")
	return supplied != "" && subtle.ConstantTimeCompare([]byte(c.Value), []byte(supplied)) == 1
}

// validOrigin rejects cross-origin browser POSTs. Non-browser clients that
// omit Origin are still protected by the CSRF token. Origins are compared by
// scheme, host and effective port rather than by string: browsers omit
// default ports from the Origin header, so an APP_BASE_URL ending in :443 or
// :80 must still match the same-origin request.
func (s *Server) validOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	base, err := url.Parse(s.cfg.AppBaseURL)
	if err != nil {
		return false
	}
	got, err := url.Parse(origin)
	if err != nil || got.Host == "" {
		return false
	}
	return strings.EqualFold(got.Scheme, base.Scheme) &&
		strings.EqualFold(got.Hostname(), base.Hostname()) &&
		effectivePort(got) == effectivePort(base)
}

func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	}
	return ""
}

// ---- flash messages ----

func (s *Server) setFlash(w http.ResponseWriter, msg string) {
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookie,
		Value:    base64.RawURLEncoding.EncodeToString([]byte(msg)),
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies(),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   120,
	})
}

func (s *Server) popFlash(w http.ResponseWriter, r *http.Request) string {
	c, err := r.Cookie(flashCookie)
	if err != nil || c.Value == "" {
		return ""
	}
	http.SetCookie(w, &http.Cookie{
		Name: flashCookie, Value: "", Path: "/", HttpOnly: true,
		Secure: s.cfg.SecureCookies(), SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return ""
	}
	return string(raw)
}

// ---- middleware ----

func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic recovered", "err", rec, "path", r.URL.Path, "stack", string(debug.Stack()))
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := r.Header.Get("X-Request-ID")
		if requestID == "" || len(requestID) > 128 {
			requestID = id.Random(12)
		}
		w.Header().Set("X-Request-ID", requestID)
		r = r.WithContext(context.WithValue(r.Context(), ctxRequestID, requestID))
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		// HSTS applies whenever the public site is HTTPS (production enables
		// Secure cookies under the same condition). Local HTTP dev is exempt
		// so localhost development is never locked into https.
		if s.cfg.SecureCookies() {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		if strings.HasPrefix(r.URL.Path, "/i/") {
			h.Set("Cross-Origin-Resource-Policy", "cross-origin")
		} else {
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' https: data:; style-src 'self'; script-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withOriginSecret(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.OriginSecret == "" || r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get("X-Origin-Secret")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.OriginSecret)) != 1 {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(rw, r)
		userID := ""
		if info := authFrom(r); info != nil && info.User != nil {
			userID = info.User.ID
		}
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"user_id", userID,
			"ip", s.clientIP(r),
			"request_id", requestIDFrom(r),
		)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

// WriteHeader records only the first status line. net/http ignores later
// WriteHeader calls (and logs "superfluous WriteHeader"), so recording the
// last one would make the access log disagree with what clients received.
func (w *statusWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.wrote = true
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Flush lets http.ResponseController stream responses through this wrapper.
func (w *statusWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Unwrap exposes the underlying ResponseWriter to http.ResponseController.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func requestIDFrom(r *http.Request) string {
	v, _ := r.Context().Value(ctxRequestID).(string)
	return v
}

func (s *Server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	remoteIP, parseErr := netip.ParseAddr(strings.TrimSpace(host))
	if parseErr == nil && s.cfg.TrustProxy && s.cfg.ClientIPHeader != "" && s.cfg.IsTrustedProxy(remoteIP) {
		if v := strings.TrimSpace(r.Header.Get(s.cfg.ClientIPHeader)); v != "" {
			for _, candidate := range strings.Split(v, ",") {
				if ip, err := netip.ParseAddr(strings.TrimSpace(candidate)); err == nil {
					return ip.String()
				}
			}
		}
	}
	if parseErr == nil {
		return remoteIP.String()
	}
	return truncate(host, 100)
}

// ---- rendering ----

type pageData struct {
	Title            string
	User             *model.User
	CSRF             string
	Flash            string
	Error            string
	Email            string
	Invite           string
	OpenRegistration bool
	AppBaseURL       string
	MaxUploadBytes   int64
	Images           []imageView
	ImageCount       int
	QuotaPercent     int
	Tokens           []model.APIToken
	NewToken         string
	Buckets          []model.StorageBucket
	BucketEncryption bool
	Stats            store.Stats
	Users            []store.UserWithCount
	Invites          []model.Invite
	Audit            []model.AuditLog
	NewInvite        string
	Contact          string
}

type imageView struct {
	model.Image
	URL      string
	Name     string
	Markdown string
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data pageData) {
	if data.User == nil {
		data.User = userFrom(r)
	}
	data.AppBaseURL = s.cfg.AppBaseURL
	data.MaxUploadBytes = s.cfg.MaxUploadBytes
	data.Contact = s.cfg.PublicContact
	if data.CSRF == "" {
		data.CSRF = s.ensureCSRF(w, r)
	}
	if data.Flash == "" {
		data.Flash = s.popFlash(w, r)
	}
	var buf strings.Builder
	if err := s.tmpl.ExecuteTemplate(&buf, name+".html", data); err != nil {
		s.log.Error("render template", "template", name, "err", err)
		http.Error(w, "template error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(buf.String()))
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Details any    `json:"details,omitempty"`
	} `json:"error"`
}

func writeAPIError(w http.ResponseWriter, status int, code, message string, details any) {
	var e apiError
	e.Error.Code = code
	e.Error.Message = message
	e.Error.Details = details
	writeJSON(w, status, e)
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 4 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

func formatTime(ts int64) string {
	return time.Unix(ts, 0).Local().Format("2006-01-02 15:04")
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

// ---- rate limiter ----

type limiter struct {
	mu          sync.Mutex
	entries     map[string]*limitEntry
	nextCleanup time.Time
}

type limitEntry struct {
	reset time.Time
	count int
}

func newLimiter() *limiter { return &limiter{entries: make(map[string]*limitEntry)} }

func (l *limiter) allow(key string, limit int, window time.Duration) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.nextCleanup.IsZero() || now.After(l.nextCleanup) {
		for k, e := range l.entries {
			if now.After(e.reset) {
				delete(l.entries, k)
			}
		}
		l.nextCleanup = now.Add(time.Minute)
	}
	e, ok := l.entries[key]
	if !ok || now.After(e.reset) {
		l.entries[key] = &limitEntry{reset: now.Add(window), count: 1}
		return true
	}
	if e.count >= limit {
		return false
	}
	e.count++
	return true
}

func tooMany(w http.ResponseWriter, retryAfter int) {
	w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
	writeAPIError(w, http.StatusTooManyRequests, "rate_limited", "请求过于频繁，请稍后再试", nil)
}
