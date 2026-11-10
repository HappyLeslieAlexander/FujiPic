package server_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"fujipic/internal/auth"
	"fujipic/internal/bucket"
	"fujipic/internal/cdn"
	"fujipic/internal/config"
	"fujipic/internal/db"
	"fujipic/internal/id"
	"fujipic/internal/model"
	"fujipic/internal/server"
	"fujipic/internal/storage"
	"fujipic/internal/store"
	"fujipic/internal/upload"
)

type testEnv struct {
	ts            *httptest.Server
	client        *http.Client
	jar           http.CookieJar
	cfg           *config.Config
	store         *store.Store
	objects       *storage.Memory
	customObjects map[string]*storage.Memory
	buckets       *bucket.Registry
	uploads       *upload.Service
}

const testStorageEncryptionKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=" // 32 bytes

func newTestEnv(t *testing.T, mutate func(*config.Config)) *testEnv {
	t.Helper()
	return newTestEnvWithLogger(t, mutate, slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func newTestEnvWithLogger(t *testing.T, mutate func(*config.Config), handler slog.Handler) *testEnv {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		AppEnv:                "development",
		AppBaseURL:            "http://example.test",
		ListenAddr:            ":0",
		DatabasePath:          filepath.Join(dir, "test.db"),
		TmpDir:                filepath.Join(dir, "tmp"),
		StorageDriver:         "memory",
		PublicBaseURL:         "http://example.test",
		MaxUploadBytes:        1 << 20,
		MaxImagePixels:        50_000_000,
		MaxImageDimension:     12000,
		DefaultQuotaBytes:     10 << 20,
		AllowOpenRegistration: true,
		SessionTTL:            time.Hour,
		ClientIPHeader:        "CF-Connecting-IP",
		CDNPurgeProvider:      "none",
		LogLevel:              "error",
		StorageEncryptionKey:  testStorageEncryptionKey,
	}
	if mutate != nil {
		mutate(cfg)
	}
	database, err := db.Open(context.Background(), cfg.DatabasePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	st := store.New(database)
	objects := storage.NewMemory()
	customObjects := make(map[string]*storage.Memory)
	buckets, err := bucket.NewRegistry(st, objects, cfg.StorageEncryptionKey, func(ctx context.Context, b model.StorageBucket, secret string) (storage.Store, error) {
		m := storage.NewMemory()
		customObjects[b.ID] = m
		return m, nil
	}, false)
	if err != nil {
		t.Fatalf("new bucket registry: %v", err)
	}
	up := &upload.Service{
		Store:      st,
		Buckets:    buckets,
		PublicBase: cfg.PublicBaseURL,
		TmpDir:     cfg.TmpDir,
		MaxBytes:   cfg.MaxUploadBytes,
		MaxPixels:  cfg.MaxImagePixels,
		MaxDim:     cfg.MaxImageDimension,
		Log:        slog.New(handler),
	}
	srv, err := server.New(cfg, st, buckets, up, cdn.None{}, slog.New(handler))
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	cfg.AppBaseURL = ts.URL
	cfg.PublicBaseURL = ts.URL
	up.PublicBase = ts.URL
	jar, _ := cookiejar.New(nil)
	return &testEnv{
		ts: ts, jar: jar, client: &http.Client{Jar: jar},
		cfg: cfg, store: st, objects: objects, customObjects: customObjects, buckets: buckets, uploads: up,
	}
}

func (e *testEnv) newClient() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar}
}

func (e *testEnv) csrf(t *testing.T, c *http.Client) string {
	t.Helper()
	resp, err := c.Get(e.ts.URL + "/login")
	if err != nil {
		t.Fatalf("get login: %v", err)
	}
	resp.Body.Close()
	u, _ := url.Parse(e.ts.URL)
	for _, cookie := range c.Jar.Cookies(u) {
		if cookie.Name == "fujipic_csrf" {
			return cookie.Value
		}
	}
	t.Fatal("csrf cookie not set")
	return ""
}

func (e *testEnv) register(t *testing.T, c *http.Client, email, password, invite string) {
	t.Helper()
	csrf := e.csrf(t, c)
	form := url.Values{"csrf": {csrf}, "email": {email}, "password": {password}}
	if invite != "" {
		form.Set("invite", invite)
	}
	resp, err := c.PostForm(e.ts.URL+"/register", form)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "图库") {
		t.Fatalf("register failed: status=%d body=%s", resp.StatusCode, truncate(string(body), 300))
	}
}

func (e *testEnv) login(t *testing.T, c *http.Client, email, password string) {
	t.Helper()
	csrf := e.csrf(t, c)
	resp, err := c.PostForm(e.ts.URL+"/login", url.Values{
		"csrf": {csrf}, "email": {email}, "password": {password},
	})
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "图库") {
		t.Fatalf("login failed: status=%d body=%s", resp.StatusCode, truncate(string(body), 300))
	}
}

type uploadResult struct {
	ID           string `json:"id"`
	BucketID     string `json:"bucket_id"`
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
}

func (e *testEnv) upload(t *testing.T, c *http.Client, data []byte, filename, csrf, bearer string) (*http.Response, uploadResult) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/api/v1/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if csrf != "" {
		req.Header.Set("X-CSRF-Token", csrf)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	var out uploadResult
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func (e *testEnv) uploadToBucket(t *testing.T, c *http.Client, data []byte, filename, csrf, bucketID string) (*http.Response, uploadResult) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if bucketID != "" {
		if err := mw.WriteField("bucket", bucketID); err != nil {
			t.Fatal(err)
		}
	}
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	mw.Close()
	req, _ := http.NewRequest(http.MethodPost, e.ts.URL+"/api/v1/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	var out uploadResult
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func makePNG(t *testing.T, w, h int, noisy bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if noisy {
		buf := make([]byte, w*h*4)
		_, _ = rand.Read(buf)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				i := (y*w + x) * 4
				img.Set(x, y, color.RGBA{buf[i], buf[i+1], buf[i+2], 255})
			}
		}
	} else {
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				img.Set(x, y, color.RGBA{20, 120, 220, 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestRegisterUploadReadDelete(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t, env.client, "user@example.com", "password123", "")
	csrf := env.csrf(t, env.client)
	data := makePNG(t, 4, 4, false)

	resp, up := env.upload(t, env.client, data, "pixel.png", csrf, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201", resp.StatusCode)
	}
	if up.ID == "" || up.URL == "" || up.MIME != "image/png" || up.Width != 4 || up.Height != 4 {
		t.Fatalf("unexpected upload result: %+v", up)
	}

	getResp, err := http.Get(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if getResp.StatusCode != 200 || !bytes.Equal(body, data) {
		t.Fatalf("image GET status=%d, bytes match=%v", getResp.StatusCode, bytes.Equal(body, data))
	}
	if got := getResp.Header.Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Fatalf("Cache-Control = %q", got)
	}
	if getResp.Header.Get("Set-Cookie") != "" {
		t.Fatal("image response must not set cookies")
	}

	headResp, err := http.Head(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	headResp.Body.Close()
	if headResp.StatusCode != 200 || headResp.Header.Get("Content-Length") != fmt.Sprint(len(data)) {
		t.Fatalf("HEAD status=%d length=%s", headResp.StatusCode, headResp.Header.Get("Content-Length"))
	}

	rangeReq, _ := http.NewRequest(http.MethodGet, up.URL, nil)
	rangeReq.Header.Set("Range", "bytes=0-9")
	rangeResp, err := http.DefaultClient.Do(rangeReq)
	if err != nil {
		t.Fatal(err)
	}
	rangeBody, _ := io.ReadAll(rangeResp.Body)
	rangeResp.Body.Close()
	if rangeResp.StatusCode != http.StatusPartialContent || len(rangeBody) != 10 {
		t.Fatalf("range status=%d len=%d", rangeResp.StatusCode, len(rangeBody))
	}
	if !strings.HasPrefix(rangeResp.Header.Get("Content-Range"), "bytes 0-9/") {
		t.Fatalf("Content-Range = %q", rangeResp.Header.Get("Content-Range"))
	}

	condReq, _ := http.NewRequest(http.MethodGet, up.URL, nil)
	condReq.Header.Set("If-None-Match", getResp.Header.Get("ETag"))
	condResp, err := http.DefaultClient.Do(condReq)
	if err != nil {
		t.Fatal(err)
	}
	condResp.Body.Close()
	if condResp.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional status = %d, want 304", condResp.StatusCode)
	}

	// Per-user deduplication returns the same image.
	dupResp, dup := env.upload(t, env.client, data, "again.png", csrf, "")
	if dupResp.StatusCode != http.StatusOK || !dup.Deduplicated || dup.ID != up.ID {
		t.Fatalf("dedup status=%d result=%+v", dupResp.StatusCode, dup)
	}

	// List contains one image.
	listResp, err := env.client.Get(env.ts.URL + "/api/v1/images")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	_ = json.NewDecoder(listResp.Body).Decode(&list)
	listResp.Body.Close()
	if len(list.Items) != 1 || list.Items[0].ID != up.ID {
		t.Fatalf("list = %+v", list)
	}

	// Delete, then the CDN origin returns 404.
	delReq, _ := http.NewRequest(http.MethodDelete, env.ts.URL+"/api/v1/images/"+up.ID, nil)
	delReq.Header.Set("X-CSRF-Token", csrf)
	delResp, err := env.client.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", delResp.StatusCode)
	}
	after, _ := http.Get(up.URL)
	after.Body.Close()
	if after.StatusCode != http.StatusNotFound {
		t.Fatalf("deleted image status = %d, want 404", after.StatusCode)
	}
	if cc := after.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("deleted image Cache-Control = %q", cc)
	}

	// Idempotent delete.
	delReq2, _ := http.NewRequest(http.MethodDelete, env.ts.URL+"/api/v1/images/"+up.ID, nil)
	delReq2.Header.Set("X-CSRF-Token", csrf)
	delResp2, _ := env.client.Do(delReq2)
	delResp2.Body.Close()
	if delResp2.StatusCode != http.StatusNoContent {
		t.Fatalf("repeat delete status = %d", delResp2.StatusCode)
	}

	purges, err := env.store.PendingCDNPurges(context.Background(), 10)
	if err != nil || len(purges) != 1 {
		t.Fatalf("cdn purges = %d, err=%v", len(purges), err)
	}
}

func TestCSRFAndTokenScopes(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t, env.client, "user@example.com", "password123", "")
	csrf := env.csrf(t, env.client)
	data := makePNG(t, 2, 2, false)

	// Cookie auth without CSRF is rejected.
	resp, _ := env.upload(t, env.client, data, "x.png", "", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no-csrf upload status = %d, want 403", resp.StatusCode)
	}

	// No auth is rejected.
	resp, _ = env.upload(t, env.newClient(), data, "x.png", "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous upload status = %d, want 401", resp.StatusCode)
	}

	// Create a token through the cookie-authenticated API.
	payload := strings.NewReader(`{"name":"script","scopes":"upload,read"}`)
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/api/v1/tokens", payload)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	tokResp, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var tokenBody struct {
		Token string `json:"token"`
		ID    string `json:"id"`
	}
	_ = json.NewDecoder(tokResp.Body).Decode(&tokenBody)
	tokResp.Body.Close()
	if tokResp.StatusCode != http.StatusCreated || !strings.HasPrefix(tokenBody.Token, "fp_") {
		t.Fatalf("create token status=%d body=%+v", tokResp.StatusCode, tokenBody)
	}

	// Bearer upload works without CSRF.
	resp, up := env.upload(t, env.newClient(), data, "x.png", "", tokenBody.Token)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("bearer upload status = %d", resp.StatusCode)
	}

	// Delete is not in the scope.
	delReq, _ := http.NewRequest(http.MethodDelete, env.ts.URL+"/api/v1/images/"+up.ID, nil)
	delReq.Header.Set("Authorization", "Bearer "+tokenBody.Token)
	delResp, _ := http.DefaultClient.Do(delReq)
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusForbidden {
		t.Fatalf("delete with upload/read token = %d, want 403", delResp.StatusCode)
	}

	// Revoking the token makes it unusable.
	delTok, _ := http.NewRequest(http.MethodDelete, env.ts.URL+"/api/v1/tokens/"+tokenBody.ID, nil)
	delTok.Header.Set("X-CSRF-Token", csrf)
	delTokResp, _ := env.client.Do(delTok)
	delTokResp.Body.Close()
	if delTokResp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke token status = %d", delTokResp.StatusCode)
	}
	meReq, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/api/v1/me", nil)
	meReq.Header.Set("Authorization", "Bearer "+tokenBody.Token)
	meResp, _ := http.DefaultClient.Do(meReq)
	meResp.Body.Close()
	if meResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token status = %d, want 401", meResp.StatusCode)
	}
}

func TestUserIsolation(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t, env.client, "a@example.com", "password123", "")
	csrfA := env.csrf(t, env.client)
	_, up := env.upload(t, env.client, makePNG(t, 3, 3, false), "a.png", csrfA, "")
	if up.ID == "" {
		t.Fatal("upload failed")
	}

	clientB := env.newClient()
	env.register(t, clientB, "b@example.com", "password123", "")
	getReq, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/api/v1/images/"+up.ID, nil)
	getResp, _ := clientB.Do(getReq)
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-user GET = %d, want 404", getResp.StatusCode)
	}
	csrfB := env.csrf(t, clientB)
	delReq, _ := http.NewRequest(http.MethodDelete, env.ts.URL+"/api/v1/images/"+up.ID, nil)
	delReq.Header.Set("X-CSRF-Token", csrfB)
	delResp, _ := clientB.Do(delReq)
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-user DELETE = %d, want 404", delResp.StatusCode)
	}
	listResp, _ := clientB.Get(env.ts.URL + "/api/v1/images")
	var list struct {
		Items []any `json:"items"`
	}
	_ = json.NewDecoder(listResp.Body).Decode(&list)
	listResp.Body.Close()
	if len(list.Items) != 0 {
		t.Fatalf("user B sees %d images", len(list.Items))
	}
}

func TestQuotaEnforced(t *testing.T) {
	env := newTestEnv(t, func(c *config.Config) { c.DefaultQuotaBytes = 200 })
	env.register(t, env.client, "q@example.com", "password123", "")
	csrf := env.csrf(t, env.client)
	data := makePNG(t, 32, 32, true) // noisy image, well over 200 bytes
	if len(data) <= 200 {
		t.Fatalf("test image too small: %d", len(data))
	}
	resp, body := env.upload(t, env.client, data, "big.png", csrf, "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("quota upload status = %d, want 403", resp.StatusCode)
	}
	_ = body
	user, err := env.store.UserByEmail(context.Background(), "q@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if user.UsedBytes != 0 {
		t.Fatalf("used_bytes = %d after failed upload, want 0", user.UsedBytes)
	}
	if env.objects.Len() != 0 {
		t.Fatalf("bucket has %d objects after failed upload", env.objects.Len())
	}
}

func TestInvalidUploads(t *testing.T) {
	env := newTestEnv(t, func(c *config.Config) { c.MaxUploadBytes = 500 })
	env.register(t, env.client, "i@example.com", "password123", "")
	csrf := env.csrf(t, env.client)

	resp, _ := env.upload(t, env.client, []byte("hello, not an image"), "fake.png", csrf, "")
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("text upload status = %d, want 415", resp.StatusCode)
	}
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	resp, _ = env.upload(t, env.client, svg, "x.svg", csrf, "")
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("svg upload status = %d, want 415", resp.StatusCode)
	}
	big := makePNG(t, 64, 64, true)
	if len(big) <= 500 {
		t.Fatalf("test image too small: %d", len(big))
	}
	resp, _ = env.upload(t, env.client, big, "big.png", csrf, "")
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize upload status = %d, want 413", resp.StatusCode)
	}
}

func TestInviteRegistration(t *testing.T) {
	env := newTestEnv(t, func(c *config.Config) { c.AllowOpenRegistration = false })
	csrf := env.csrf(t, env.client)
	resp, err := env.client.PostForm(env.ts.URL+"/register", url.Values{
		"csrf": {csrf}, "email": {"x@example.com"}, "password": {"password123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "需要邀请码") {
		t.Fatalf("missing invite error, body=%s", truncate(string(body), 300))
	}

	invite := &model.Invite{Code: "invite-test-1", CreatedAt: time.Now().Unix()}
	if err := env.store.CreateInvite(context.Background(), invite); err != nil {
		t.Fatal(err)
	}
	env.register(t, env.client, "x@example.com", "password123", invite.Code)

	// The invite cannot be reused.
	client2 := env.newClient()
	csrf2 := env.csrf(t, client2)
	resp2, _ := client2.PostForm(env.ts.URL+"/register", url.Values{
		"csrf": {csrf2}, "email": {"y@example.com"}, "password": {"password123"}, "invite": {invite.Code},
	})
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if !strings.Contains(string(body2), "邀请码无效") {
		t.Fatalf("reused invite not rejected: %s", truncate(string(body2), 300))
	}
}

func TestOriginSecretAndImageKeyValidation(t *testing.T) {
	env := newTestEnv(t, func(c *config.Config) { c.OriginSecret = "top-secret" })

	// Health checks stay reachable for the CDN / load balancer.
	resp, err := http.Get(env.ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}

	// Everything else requires the shared secret.
	resp, err = http.Get(env.ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("login without secret = %d, want 403", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/login", nil)
	req.Header.Set("X-Origin-Secret", "top-secret")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login with secret = %d, want 200", resp.StatusCode)
	}

	for _, path := range []string{"/i/foo", "/i/2026/11/notavalidkey.png", "/i/2026/11/aaaaaaaaaaaaaaaaaaaaaaaaaa.png"} {
		req, _ := http.NewRequest(http.MethodGet, env.ts.URL+path, nil)
		req.Header.Set("X-Origin-Secret", "top-secret")
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		if r.StatusCode != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404", path, r.StatusCode)
		}
	}
}

func TestAdminLastAdminProtection(t *testing.T) {
	env := newTestEnv(t, nil)
	hash, _ := auth.HashPassword("admin-password-123")
	now := time.Now().Unix()
	admin := &model.User{
		ID: id.New("usr"), Email: "admin@example.com", PasswordHash: hash,
		Role: "admin", Status: "active", QuotaBytes: 1 << 20, CreatedAt: now, UpdatedAt: now,
	}
	if err := env.store.CreateUser(context.Background(), admin); err != nil {
		t.Fatal(err)
	}
	env.login(t, env.client, "admin@example.com", "admin-password-123")
	csrf := env.csrf(t, env.client)
	resp, err := env.client.PostForm(env.ts.URL+"/admin/users/"+admin.ID+"/status", url.Values{
		"csrf": {csrf}, "status": {"disabled"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "不能禁用最后一个管理员") {
		t.Fatalf("last admin protection missing: %s", truncate(string(body), 300))
	}
	user, _ := env.store.UserByID(context.Background(), admin.ID)
	if user.Status != "active" {
		t.Fatalf("admin status = %q, want active", user.Status)
	}
}

// TestCSRFTokenRotatesOnLoginAndClearsOnLogout pins SEC-03: signing in
// moves to a fresh CSRF value, and the browser credential is retired at
// logout rather than surviving with the cookie jar.
func TestCSRFTokenRotatesOnLoginAndClearsOnLogout(t *testing.T) {
	env := newTestEnv(t, nil)
	c := env.newClient()
	before := env.csrf(t, c)

	registerResp, err := c.PostForm(env.ts.URL+"/register", url.Values{
		"csrf": {before}, "email": {"csrf@example.com"}, "password": {"password123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	registerResp.Body.Close()

	u, _ := url.Parse(env.ts.URL)
	after := ""
	for _, cookie := range c.Jar.Cookies(u) {
		if cookie.Name == "fujipic_csrf" {
			after = cookie.Value
		}
	}
	if after == "" || after == before {
		t.Fatalf("CSRF not rotated on authenticated session creation: before=%q after=%q", before, after)
	}

	noRedirect := &http.Client{Jar: c.Jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	logoutReq, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/logout", strings.NewReader("csrf="+after))
	logoutReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	logoutResp, err := noRedirect.Do(logoutReq)
	if err != nil {
		t.Fatal(err)
	}
	logoutResp.Body.Close()
	if logoutResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout status = %d, want 303", logoutResp.StatusCode)
	}
	setCookies := strings.Join(logoutResp.Header.Values("Set-Cookie"), "; ")
	if !strings.Contains(setCookies, "fujipic_csrf=;") || !strings.Contains(setCookies, "Max-Age=0") {
		t.Fatalf("logout does not expire CSRF cookie: %q", setCookies)
	}
}

func TestHealthAndReady(t *testing.T) {
	env := newTestEnv(t, nil)
	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := http.Get(env.ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d", path, resp.StatusCode)
		}
	}
}

func TestEmailNormalization(t *testing.T) {
	env := newTestEnv(t, nil)
	// Register with a display-name email form; the stored address must be the
	// bare lowercase address.
	env.register(t, env.client, "Alice <Alice@Example.com>", "password123", "")
	user, err := env.store.UserByEmail(context.Background(), "alice@example.com")
	if err != nil {
		t.Fatalf("normalized user not found: %v", err)
	}
	if user.Email != "alice@example.com" {
		t.Fatalf("stored email = %q, want bare lowercase address", user.Email)
	}
	// A fresh client can log in with the bare address in any case.
	client2 := env.newClient()
	env.login(t, client2, "ALICE@Example.COM", "password123")
	// Login with the display-name form must also work.
	client3 := env.newClient()
	env.login(t, client3, "Alice <alice@example.com>", "password123")
	// RFC 5322 reply-to style address with a display name must not be rejected.
}

func TestStaticCacheHeader(t *testing.T) {
	env := newTestEnv(t, nil)
	for _, path := range []string{"/static/app.css", "/static/app.js"} {
		resp, err := http.Get(env.ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if len(body) == 0 {
			t.Fatalf("%s returned empty body", path)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s = %d, want 200", path, resp.StatusCode)
		}
		cc := resp.Header.Get("Cache-Control")
		if !strings.Contains(cc, "max-age=604800") {
			t.Fatalf("%s Cache-Control = %q, want max-age=604800", path, cc)
		}
	}
}

// TestNoStoreAndCookieFreeHeaders locks the origin-side contract that the CDN
// relies on: API and web pages must never be cached (no-store, no
// CDN-Cache-Control), while image bytes must be cacheable and cookie-free.
func TestNoStoreAndCookieFreeHeaders(t *testing.T) {
	env := newTestEnv(t, nil)
	checkNoStore := func(resp *http.Response, name string) {
		t.Helper()
		if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", name, cc)
		}
		if cdn := resp.Header.Get("CDN-Cache-Control"); cdn != "" {
			t.Errorf("%s: CDN-Cache-Control = %q, want empty", name, cdn)
		}
	}
	checkNoCookie := func(resp *http.Response, name string) {
		t.Helper()
		if cookies := resp.Header.Values("Set-Cookie"); len(cookies) != 0 {
			t.Errorf("%s: Set-Cookie = %q, want none", name, cookies)
		}
	}
	get := func(c *http.Client, path string) *http.Response {
		t.Helper()
		resp, err := c.Get(env.ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		// Copy headers before returning; Body is closed.
		return resp
	}

	anon := env.newClient()
	resp := get(anon, "/login")
	checkNoStore(resp, "GET /login")

	env.register(t, env.client, "h@example.com", "password123", "")
	resp = get(env.client, "/dashboard")
	checkNoStore(resp, "GET /dashboard")

	resp = get(env.client, "/api/v1/me")
	checkNoStore(resp, "GET /api/v1/me")
	checkNoCookie(resp, "GET /api/v1/me")

	resp = get(env.client, "/api/v1/images")
	checkNoStore(resp, "GET /api/v1/images")
	checkNoCookie(resp, "GET /api/v1/images")

	csrf := env.csrf(t, env.client)
	upResp, up := env.upload(t, env.client, makePNG(t, 2, 2, false), "x.png", csrf, "")
	if upResp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201", upResp.StatusCode)
	}
	checkNoStore(upResp, "POST /api/v1/upload")
	checkNoCookie(upResp, "POST /api/v1/upload")

	// Image bytes: cacheable, CDN-Cache-Control present, cookie-free.
	imgResp, err := http.Get(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, imgResp.Body)
	imgResp.Body.Close()
	if cc := imgResp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("GET image: Cache-Control = %q, want immutable", cc)
	}
	if cdn := imgResp.Header.Get("CDN-Cache-Control"); cdn == "" {
		t.Errorf("GET image: CDN-Cache-Control empty, want present")
	}
	checkNoCookie(imgResp, "GET image")

	headResp, err := http.Head(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	headResp.Body.Close()
	checkNoCookie(headResp, "HEAD image")
	if cc := headResp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("HEAD image: Cache-Control = %q, want immutable", cc)
	}
}

// TestImageErrorResponsesAreNotStored locks the origin-side complement of
// the CDN rule: 4xx image responses carry no-store and no CDN cacheable
// directives. The actual CDN cache rule still needs environment acceptance.
func TestImageErrorResponsesAreNotStored(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t, env.client, "cache-errors@example.com", "password123", "")
	csrf := env.csrf(t, env.client)
	upResp, up := env.upload(t, env.client, makePNG(t, 2, 2, false), "x.png", csrf, "")
	upResp.Body.Close()
	if upResp.StatusCode != http.StatusCreated || up.URL == "" {
		t.Fatalf("upload status=%d url=%q", upResp.StatusCode, up.URL)
	}

	assertNotStored := func(resp *http.Response, name string) {
		t.Helper()
		if got := resp.Header.Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control=%q, want no-store", name, got)
		}
		if got := resp.Header.Get("CDN-Cache-Control"); got != "" {
			t.Errorf("%s: CDN-Cache-Control=%q, want absent", name, got)
		}
		if cookies := resp.Header.Values("Set-Cookie"); len(cookies) != 0 {
			t.Errorf("%s: Set-Cookie=%q, want none", name, cookies)
		}
	}

	rangeReq, _ := http.NewRequest(http.MethodGet, up.URL, nil)
	rangeReq.Header.Set("Range", "bytes=999999-1000000")
	rangeResp, err := http.DefaultClient.Do(rangeReq)
	if err != nil {
		t.Fatal(err)
	}
	rangeResp.Body.Close()
	if rangeResp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("invalid range status=%d, want 416", rangeResp.StatusCode)
	}
	assertNotStored(rangeResp, "invalid range")

	missingResp, err := http.Get(env.ts.URL + "/i/2026/11/aaaaaaaaaaaaaaaaaaaaaaaaaa.png")
	if err != nil {
		t.Fatal(err)
	}
	missingResp.Body.Close()
	if missingResp.StatusCode != http.StatusNotFound {
		t.Fatalf("missing image status=%d, want 404", missingResp.StatusCode)
	}
	assertNotStored(missingResp, "missing image")
}

// TestSecurityHeaders locks the origin-side hardening headers.
func TestSecurityHeaders(t *testing.T) {
	env := newTestEnv(t, nil)
	check := func(resp *http.Response, name string) {
		t.Helper()
		if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Errorf("%s: X-Content-Type-Options = %q, want nosniff", name, got)
		}
		if got := resp.Header.Get("X-Frame-Options"); got != "DENY" {
			t.Errorf("%s: X-Frame-Options = %q, want DENY", name, got)
		}
		if got := resp.Header.Get("Referrer-Policy"); got != "strict-origin-when-cross-origin" {
			t.Errorf("%s: Referrer-Policy = %q", name, got)
		}
		if got := resp.Header.Get("Cross-Origin-Resource-Policy"); got != "same-origin" {
			t.Errorf("%s: Cross-Origin-Resource-Policy = %q, want same-origin for non-image", name, got)
		}
		csp := resp.Header.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'self'", "script-src 'self'", "frame-ancestors 'none'", "form-action 'self'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP missing %q (got %q)", name, want, csp)
			}
		}
	}
	loginResp, err := http.Get(env.ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	loginResp.Body.Close()
	check(loginResp, "GET /login")

	env.register(t, env.client, "sec@example.com", "password123", "")
	meReq, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/api/v1/me", nil)
	meResp, err := env.client.Do(meReq)
	if err != nil {
		t.Fatal(err)
	}
	meResp.Body.Close()
	check(meResp, "GET /api/v1/me")

	// Image origin: nosniff + cross-origin CORP, no restrictive CSP.
	csrf := env.csrf(t, env.client)
	_, up := env.upload(t, env.client, makePNG(t, 2, 2, false), "x.png", csrf, "")
	imgResp, err := http.Get(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	imgResp.Body.Close()
	if got := imgResp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("GET image: X-Content-Type-Options = %q", got)
	}
	if got := imgResp.Header.Get("Cross-Origin-Resource-Policy"); got != "cross-origin" {
		t.Errorf("GET image: Cross-Origin-Resource-Policy = %q, want cross-origin", got)
	}
}

// TestCrossOriginCookieWritesRejected verifies that cookie-authenticated API
// writes reject foreign Origin even with a valid CSRF token, while clients
// that omit Origin (curl) keep working.
func TestCrossOriginCookieWritesRejected(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t, env.client, "x@example.com", "password123", "")
	csrf := env.csrf(t, env.client)
	data := makePNG(t, 2, 2, false)

	// Foreign Origin + valid CSRF must be rejected on upload.
	foreign := func(req *http.Request) {
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("X-CSRF-Token", csrf)
	}
	upReq := func(dst string) *http.Request {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		part, _ := mw.CreateFormFile("file", "x.png")
		_, _ = part.Write(data)
		mw.Close()
		req, _ := http.NewRequest(http.MethodPost, env.ts.URL+dst, &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		foreign(req)
		return req
	}
	resp, err := env.client.Do(upReq("/api/v1/upload"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign-origin upload = %d, want 403", resp.StatusCode)
	}

	// Same-origin Origin passes Origin validation and only fails later checks
	// if CSRF is wrong; here CSRF is valid so the upload succeeds.
	okReq := upReq("/api/v1/upload")
	okReq.Header.Set("Origin", env.ts.URL)
	okResp, err := env.client.Do(okReq)
	if err != nil {
		t.Fatal(err)
	}
	okResp.Body.Close()
	if okResp.StatusCode != http.StatusCreated {
		t.Fatalf("same-origin upload = %d, want 201", okResp.StatusCode)
	}

	// No-Origin (curl-style) still works because CSRF protects it.
	csrf2 := env.csrf(t, env.client)
	noOriginResp, _ := env.upload(t, env.client, data, "y.png", csrf2, "")
	if noOriginResp.StatusCode != http.StatusOK { // dedup of identical bytes
		t.Fatalf("no-origin upload = %d, want 200 (dedup)", noOriginResp.StatusCode)
	}

	// Logout (web form) with foreign Origin is rejected even with valid CSRF.
	logoutReq, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/logout", strings.NewReader("csrf="+csrf))
	logoutReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	foreign(logoutReq)
	logoutResp, err := env.client.Do(logoutReq)
	if err != nil {
		t.Fatal(err)
	}
	logoutResp.Body.Close()
	if logoutResp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign-origin logout = %d, want 403", logoutResp.StatusCode)
	}
	// Session must still be valid after the rejected logout, and the
	// dashboard must render the uploaded image (regression: template used
	// .Size instead of .SizeBytes and 500ed whenever an image existed).
	dashResp, err := env.client.Get(env.ts.URL + "/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(dashResp.Body)
	dashResp.Body.Close()
	if dashResp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", dashResp.StatusCode)
	}
	// Dedup keeps the first upload's original name (x.png).
	if html := string(body); !strings.Contains(html, "图库") || !strings.Contains(html, "x.png") {
		t.Fatal("session invalidated by rejected logout or dashboard missing uploaded image")
	}
}

// TestUploadRateLimit covers the 60/min per-user upload bucket.
func TestUploadRateLimit(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t, env.client, "rl@example.com", "password123", "")
	csrf := env.csrf(t, env.client)
	data := makePNG(t, 2, 2, false)
	var last *http.Response
	for i := 0; i < 61; i++ {
		resp, _ := env.upload(t, env.client, data, "x.png", csrf, "")
		last = resp
		if i < 60 && resp.StatusCode >= 400 {
			t.Fatalf("upload %d: status = %d, want < 400 before limit", i+1, resp.StatusCode)
		}
	}
	if last.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("61st upload = %d, want 429", last.StatusCode)
	}
	if got := last.Header.Get("Retry-After"); got == "" {
		t.Fatal("429 missing Retry-After")
	}
}

// TestOrdinaryAPIReadRateLimit covers PLAN §5.6: 300 ordinary reads per
// minute per user, shared across endpoints.
func TestOrdinaryAPIReadRateLimit(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t, env.client, "rl2@example.com", "password123", "")

	getStatus := func(path string) *http.Response {
		t.Helper()
		resp, err := env.client.Get(env.ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp
	}

	// First 300 reads succeed...
	for i := 0; i < 300; i++ {
		if resp := getStatus("/api/v1/me"); resp.StatusCode != http.StatusOK {
			t.Fatalf("read %d: status = %d, want 200", i+1, resp.StatusCode)
		}
	}
	// ...the 301st is rejected with 429 + Retry-After.
	tooMany := getStatus("/api/v1/me")
	if tooMany.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("read 301: status = %d, want 429", tooMany.StatusCode)
	}
	if got := tooMany.Header.Get("Retry-After"); got == "" {
		t.Fatal("301st read: Retry-After missing")
	}
	// The bucket is shared: other read endpoints are also limited.
	for _, path := range []string{"/api/v1/images", "/api/v1/images/img_fake"} {
		resp := getStatus(path)
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("shared bucket: GET %s = %d, want 429", path, resp.StatusCode)
		}
	}

	// Another user is unaffected: the bucket is keyed per user.
	other := env.newClient()
	env.register(t, other, "rl3@example.com", "password123", "")
	otherResp, err := other.Get(env.ts.URL + "/api/v1/me")
	if err != nil {
		t.Fatal(err)
	}
	otherResp.Body.Close()
	if otherResp.StatusCode != http.StatusOK {
		t.Fatalf("other user read = %d, want 200 (per-user bucket)", otherResp.StatusCode)
	}
}

// TestPasswordChangeRateLimit covers PLAN §5.6: 5 password-change attempts
// per hour per user. Wrong-current-password attempts count too, and the
// block holds even when a later attempt uses the correct password.
func TestPasswordChangeRateLimit(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t, env.client, "pw@example.com", "password123", "")

	attempt := func(current string) string {
		t.Helper()
		csrf := env.csrf(t, env.client)
		resp, err := env.client.PostForm(env.ts.URL+"/settings/password", url.Values{
			"csrf": {csrf}, "current": {current}, "new": {"new-password-456"}, "confirm": {"new-password-456"},
		})
		if err != nil {
			t.Fatalf("password change: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}

	for i := 0; i < 5; i++ {
		body := attempt("wrong-password")
		if !strings.Contains(body, "当前密码不正确") {
			t.Fatalf("attempt %d: want wrong-password message, got %s", i+1, truncate(body, 300))
		}
	}
	// 6th attempt is blocked regardless of whether the password is correct.
	if body := attempt("wrong-password"); !strings.Contains(body, "尝试次数过多") {
		t.Fatalf("attempt 6: want rate-limit message, got %s", truncate(body, 300))
	}
	if body := attempt("password123"); !strings.Contains(body, "尝试次数过多") {
		t.Fatalf("attempt 7 (correct password): want rate-limit message, got %s", truncate(body, 300))
	}
	// The password was never changed by the blocked attempts: a fresh
	// client can still log in with the original password.
	other := env.newClient()
	env.login(t, other, "pw@example.com", "password123")
}

// TestHSTSAndSecureCookies locks transport hardening: no HSTS on local
// HTTP dev (would lock out localhost), HSTS + Secure/HttpOnly/SameSite
// session cookies in production.
func TestHSTSAndSecureCookies(t *testing.T) {
	// Development over plain HTTP: no HSTS.
	dev := newTestEnv(t, nil)
	resp, err := http.Get(dev.ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("dev HSTS = %q, want empty", got)
	}

	prod := newTestEnv(t, func(c *config.Config) { c.AppEnv = "production" })
	resp, err = http.Get(prod.ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	hsts := resp.Header.Get("Strict-Transport-Security")
	if !strings.Contains(hsts, "max-age=31536000") || !strings.Contains(hsts, "includeSubDomains") {
		t.Fatalf("production HSTS = %q, want max-age=31536000; includeSubDomains", hsts)
	}

	// Register in production: session cookie must carry the safety flags.
	prod.register(t, prod.client, "prod@example.com", "password123", "")
	// Re-login with a no-redirect client so the Set-Cookie headers of the
	// login response itself can be inspected.
	nc := &http.Client{Jar: prod.client.Jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	csrf := prod.csrf(t, nc)
	loginResp, err := nc.PostForm(prod.ts.URL+"/login", url.Values{
		"csrf": {csrf}, "email": {"prod@example.com"}, "password": {"password123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer loginResp.Body.Close()
	cookies := loginResp.Header.Values("Set-Cookie")
	foundSession := false
	for _, c := range cookies {
		if strings.HasPrefix(c, "fujipic_session=") {
			foundSession = true
			low := strings.ToLower(c)
			for _, want := range []string{"secure", "httponly", "samesite"} {
				if !strings.Contains(low, want) {
					t.Errorf("session cookie missing %q: %s", want, c)
				}
			}
		}
	}
	if !foundSession {
		t.Fatalf("no session Set-Cookie in login response: %q", cookies)
	}
	if loginResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status = %d, want 303", loginResp.StatusCode)
	}
}

// TestLoginUniformErrors pins PLAN §5.6/1207 behaviour: unknown emails and
// wrong passwords produce the same status and message, so responses do not
// reveal whether an account exists. Timing equalisation itself comes from
// auth.DummyVerify on the unknown-email path; that is code-level, while
// this test locks the observable contract.
func TestLoginUniformErrors(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t, env.client, "user@example.com", "password123", "")

	attempt := func(email, password string) string {
		t.Helper()
		c := env.newClient()
		csrf := env.csrf(t, c)
		resp, err := c.PostForm(env.ts.URL+"/login", url.Values{
			"csrf": {csrf}, "email": {email}, "password": {password},
		})
		if err != nil {
			t.Fatalf("login attempt %s: %v", email, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("login %s: status = %d, want 200 (rendered error page)", email, resp.StatusCode)
		}
		return string(body)
	}

	for name, body := range map[string]string{
		"unknown email":  attempt("ghost@example.com", "password123"),
		"wrong password": attempt("user@example.com", "wrong-password"),
	} {
		if !strings.Contains(body, "邮箱或密码不正确") {
			t.Errorf("%s: missing unified error message in %s", name, truncate(body, 300))
		}
		// Must not state whether the account exists. (The submitted email
		// itself is echoed back in the form, which reveals nothing.)
		for _, leak := range []string{"用户不存在", "未注册", "账号不存在"} {
			if strings.Contains(body, leak) {
				t.Errorf("%s: body leaks existence info (%q)", name, leak)
			}
		}
	}
	// Parameterized-lookup probe: an injection string must be treated as an
	// ordinary unknown email, never as a bypass.
	body := attempt("' OR '1'='1", "anything-123")
	if !strings.Contains(body, "邮箱或密码不正确") {
		t.Fatalf("injection probe: unexpected body %s", truncate(body, 300))
	}
}

// TestUploadEscapesUserFilename exercises the escaping hardening end to
// end: an <script>/quote-bearing multipart filename must not survive raw in
// the HTML embed returned by the API.
func TestUploadEscapesUserFilename(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t, env.client, "esc@example.com", "password123", "")
	csrf := env.csrf(t, env.client)
	// (A payload containing '/' would be reduced by filepath.Base traversal
	// protection; this one isolates the escaping layer itself.)
	payload := `<img src=x onerror=alert(1)>.png`
	_, up := env.upload(t, env.client, makePNG(t, 2, 2, false), payload, csrf, "")
	t.Logf("name=%q markdown=%q html=%q", up.Name, up.Markdown, up.HTML)
	if up.HTML == "" || strings.Contains(up.HTML, "<img src=x") {
		t.Fatalf("html embed not escaped: %s", up.HTML)
	}
	if !strings.Contains(up.HTML, "&lt;img") {
		t.Fatalf("html embed missing escaped form: %s", up.HTML)
	}
	if got := up.Name; got != payload {
		t.Fatalf("stored name = %q, want original %q", got, payload)
	}
	if strings.Contains(up.URL, "<") || strings.Contains(up.URL, ">") {
		t.Fatalf("url escaped-injection: %s", up.URL)
	}
}

// TestAdminCannotReadOthersImages pins PLAN 1221: administrators get no
// privilege over other users' images. Cross-user reads/deletes are a
// uniform 404, same as for ordinary users.
func TestAdminCannotReadOthersImages(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t, env.client, "owner@example.com", "password123", "")
	csrfOwner := env.csrf(t, env.client)
	_, up := env.upload(t, env.client, makePNG(t, 2, 2, false), "owner.png", csrfOwner, "")
	if up.ID == "" {
		t.Fatal("owner upload failed")
	}

	// Admin user created directly in the store (no bootstrap needed).
	hash, err := auth.HashPassword("admin-password-123")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	admin := &model.User{
		ID: id.New("usr"), Email: "admin@example.com", PasswordHash: hash,
		Role: "admin", Status: "active", QuotaBytes: 10 << 20, CreatedAt: now, UpdatedAt: now,
	}
	if err := env.store.CreateUser(context.Background(), admin); err != nil {
		t.Fatal(err)
	}
	adminClient := env.newClient()
	env.login(t, adminClient, "admin@example.com", "admin-password-123")
	csrfAdmin := env.csrf(t, adminClient)

	// Admin GET on the owner's image is a uniform 404.
	getReq, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/api/v1/images/"+up.ID, nil)
	getResp, err := adminClient.Do(getReq)
	if err != nil {
		t.Fatal(err)
	}
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusNotFound {
		t.Fatalf("admin GET other image = %d, want 404", getResp.StatusCode)
	}

	// Admin DELETE is also refused.
	delReq, _ := http.NewRequest(http.MethodDelete, env.ts.URL+"/api/v1/images/"+up.ID, nil)
	delReq.Header.Set("X-CSRF-Token", csrfAdmin)
	delResp, err := adminClient.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNotFound {
		t.Fatalf("admin DELETE other image = %d, want 404", delResp.StatusCode)
	}

	// Admin list shows none of the owner's images.
	listResp, err := adminClient.Get(env.ts.URL + "/api/v1/images")
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Items []any `json:"items"`
	}
	_ = json.NewDecoder(listResp.Body).Decode(&list)
	listResp.Body.Close()
	if len(list.Items) != 0 {
		t.Fatalf("admin list sees %d images", len(list.Items))
	}

	// Owner still sees the untouched image.
	ownResp, err := env.client.Get(env.ts.URL + "/api/v1/images/" + up.ID)
	if err != nil {
		t.Fatal(err)
	}
	ownResp.Body.Close()
	if ownResp.StatusCode != http.StatusOK {
		t.Fatalf("owner GET own image = %d, want 200", ownResp.StatusCode)
	}
}

// TestBearerCannotAccessWebConsole pins SEC-02: a scoped API credential
// stays an API credential. It cannot be used as a browser session to read
// dashboard or admin pages, even when it belongs to an administrator.
func TestBearerCannotAccessWebConsole(t *testing.T) {
	env := newTestEnv(t, nil)
	hash, err := auth.HashPassword("admin-password-123")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	admin := &model.User{
		ID: id.New("usr"), Email: "admin-token-case@example.com", PasswordHash: hash,
		Role: "admin", Status: "active", QuotaBytes: 10 << 20, CreatedAt: now, UpdatedAt: now,
	}
	if err := env.store.CreateUser(context.Background(), admin); err != nil {
		t.Fatal(err)
	}
	plaintext, tokenHash, prefix := auth.NewToken()
	if err := env.store.CreateToken(context.Background(), &model.APIToken{
		ID: id.New("tok"), UserID: admin.ID, Name: "upload-only",
		TokenHash: tokenHash, TokenPrefix: prefix, Scopes: "upload",
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/dashboard", "/settings", "/admin"} {
		req, _ := http.NewRequest(http.MethodGet, env.ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+plaintext)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s with Bearer = %d, want 403", path, resp.StatusCode)
		}
	}

	// Login page behavior stays usable: Bearer does not turn the HTML login
	// route into a dashboard redirect.
	loginReq, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/login", nil)
	loginReq.Header.Set("Authorization", "Bearer "+plaintext)
	loginResp, err := http.DefaultClient.Do(loginReq)
	if err != nil {
		t.Fatal(err)
	}
	loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("Bearer GET /login = %d, want 200 login page", loginResp.StatusCode)
	}
}

// TestUploadMimeSpoofingIgnored pins PLAN 1211: the stored MIME/extension
// and the delivered Content-Type come from the decoded bytes, never from
// the client-supplied filename.
func TestUploadMimeSpoofingIgnored(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t, env.client, "spoof@example.com", "password123", "")
	csrf := env.csrf(t, env.client)

	check := func(data []byte, filename, wantMIME, wantExt string) {
		t.Helper()
		resp, up := env.upload(t, env.client, data, filename, csrf, "")
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("upload %s: status = %d, want 201", filename, resp.StatusCode)
		}
		if up.MIME != wantMIME {
			t.Fatalf("upload %s: mime = %q, want %q", filename, up.MIME, wantMIME)
		}
		if !strings.HasSuffix(up.URL, wantExt) {
			t.Fatalf("upload %s: url = %q, want suffix %q", filename, up.URL, wantExt)
		}
		imgResp, err := http.Get(up.URL)
		if err != nil {
			t.Fatal(err)
		}
		imgResp.Body.Close()
		if got := imgResp.Header.Get("Content-Type"); got != wantMIME {
			t.Fatalf("GET %s image Content-Type = %q, want %q", filename, got, wantMIME)
		}
	}

	check(makePNG(t, 3, 3, false), "evil.svg", "image/png", ".png")
	jpeg := makeJPEG(t, 3, 3)
	check(jpeg, "photo.png", "image/jpeg", ".jpg")
	// 4x4 has a different SHA-256, avoiding the per-user dedup path.
	check(makeJPEG(t, 4, 4), "photo.exe", "image/jpeg", ".jpg")

	// Reverse spoof: non-image bytes under an image extension is rejected.
	resp, _ := env.upload(t, env.client, []byte("plain text, not an image"), "photo.png", csrf, "")
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("text-as-png status = %d, want 415", resp.StatusCode)
	}
}

func makeJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{30, 90, 200, 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestUploadDimensionLimits pins PLAN 1210 at the HTTP layer: over-pixel
// and over-dimension uploads are rejected with 422 image_too_large (the
// byte-size 413 and type 415 cases are covered by TestInvalidUploads).
func TestUploadDimensionLimits(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*config.Config)
	}{
		{"pixels", func(c *config.Config) { c.MaxImagePixels = 100 }},
		{"dimension", func(c *config.Config) { c.MaxImageDimension = 10 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, tc.mutate)
			env.register(t, env.client, "dim@example.com", "password123", "")
			csrf := env.csrf(t, env.client)
			data := makePNG(t, 64, 64, false) // 64x64: over pixel and dimension limits

			var buf bytes.Buffer
			mw := multipart.NewWriter(&buf)
			part, err := mw.CreateFormFile("file", "dim.png")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := part.Write(data); err != nil {
				t.Fatal(err)
			}
			mw.Close()
			req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/api/v1/upload", &buf)
			req.Header.Set("Content-Type", mw.FormDataContentType())
			req.Header.Set("X-CSRF-Token", csrf)
			resp, err := env.client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("%s: status = %d, want 422 (body %s)", tc.name, resp.StatusCode, truncate(string(body), 200))
			}
			var apiErr struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(body, &apiErr); err != nil || apiErr.Error.Code != "image_too_large" {
				t.Fatalf("%s: error code = %q, want image_too_large (body %s)", tc.name, apiErr.Error.Code, truncate(string(body), 200))
			}
		})
	}
}

// TestUploadObjectKey pins PLAN 1212: object keys are server-generated
// random values, so a path-traversal filename cannot influence the key or
// the delivered bytes. Two distinct uploads must get distinct keys.
func TestUploadObjectKey(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t, env.client, "key@example.com", "password123", "")
	csrf := env.csrf(t, env.client)

	keyRe := regexp.MustCompile(`^i/\d{4}/\d{2}/[a-z2-7]{26}\.png$`)
	urlPath := func(rawURL string) string {
		u, err := url.Parse(rawURL)
		if err != nil {
			t.Fatalf("parse url %q: %v", rawURL, err)
		}
		return strings.TrimPrefix(u.Path, "/")
	}

	resp1, first := env.upload(t, env.client, makePNG(t, 3, 3, true), "../../../../tmp/evil.png", csrf, "")
	if resp1.StatusCode != http.StatusCreated {
		t.Fatalf("traversal upload status = %d, want 201", resp1.StatusCode)
	}
	if !keyRe.MatchString(urlPath(first.URL)) {
		t.Fatalf("key in url %q does not match %q", first.URL, keyRe)
	}
	if first.Name != "evil.png" {
		t.Fatalf("display name = %q, want evil.png (Base reduction)", first.Name)
	}
	imgResp, err := http.Get(first.URL)
	if err != nil {
		t.Fatal(err)
	}
	imgResp.Body.Close()
	if imgResp.StatusCode != http.StatusOK || imgResp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("GET image status=%d type=%q", imgResp.StatusCode, imgResp.Header.Get("Content-Type"))
	}
	if imgResp.Header.Get("Set-Cookie") != "" {
		t.Fatal("image response must not set cookies")
	}

	resp2, second := env.upload(t, env.client, makePNG(t, 5, 5, true), "normal.png", csrf, "")
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("second upload status = %d, want 201", resp2.StatusCode)
	}
	if second.URL == first.URL {
		t.Fatalf("two distinct images share key %q", second.URL)
	}
	if !keyRe.MatchString(urlPath(second.URL)) {
		t.Fatalf("second key %q does not match %q", second.URL, keyRe)
	}
}

// TestLogSanitization pins PLAN 1223: access logs must not contain secrets
// (passwords, bearer tokens, cookies) and API error bodies must not leak
// internal filesystem paths, DB engine text or stack traces.
func TestLogSanitization(t *testing.T) {
	var buf bytes.Buffer
	env := newTestEnvWithLogger(t, nil, slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))

	const secretPassword = "password123"
	env.register(t, env.client, "log@example.com", secretPassword, "")
	csrf := env.csrf(t, env.client)

	// Create an API token and use it, so the run contains real secrets.
	payload := strings.NewReader(`{"name":"t","scopes":"upload,read"}`)
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/api/v1/tokens", payload)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	tokResp, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var tokenBody struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(tokResp.Body).Decode(&tokenBody)
	tokResp.Body.Close()
	if tokenBody.Token == "" {
		t.Fatal("token creation failed")
	}

	resp, up := env.upload(t, env.newClient(), makePNG(t, 2, 2, false), "x.png", "", tokenBody.Token)
	if resp.StatusCode != http.StatusCreated || up.ID == "" {
		t.Fatalf("bearer upload status = %d", resp.StatusCode)
	}

	// Wrong login, then an unsupported-type upload to exercise error paths.
	anon := env.newClient()
	anonCSRF := env.csrf(t, anon)
	wrongResp, err := anon.PostForm(env.ts.URL+"/login", url.Values{
		"csrf": {anonCSRF}, "email": {"log@example.com"}, "password": {"wrong-password"},
	})
	if err != nil {
		t.Fatal(err)
	}
	wrongResp.Body.Close()

	badBuf := &bytes.Buffer{}
	mw := multipart.NewWriter(badBuf)
	part, _ := mw.CreateFormFile("file", "fake.png")
	_, _ = part.Write([]byte("not an image"))
	mw.Close()
	badReq, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/api/v1/upload", badBuf)
	badReq.Header.Set("Content-Type", mw.FormDataContentType())
	badReq.Header.Set("X-CSRF-Token", csrf)
	badResp, err := env.client.Do(badReq)
	if err != nil {
		t.Fatal(err)
	}
	defer badResp.Body.Close()
	badBody, _ := io.ReadAll(badResp.Body)
	if badResp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("bad upload status = %d, want 415", badResp.StatusCode)
	}
	for _, marker := range []string{"sqlite", "/Users", "fujipic.db", "panic", "goroutine"} {
		if strings.Contains(string(badBody), marker) {
			t.Fatalf("error body leaks %q: %s", marker, truncate(string(badBody), 200))
		}
	}

	logText := buf.String()
	if !strings.Contains(logText, "request") {
		t.Fatalf("expected access-log lines, got %s", truncate(logText, 500))
	}
	for _, secret := range []string{secretPassword, "wrong-password", tokenBody.Token, "Bearer", "Cookie"} {
		if strings.Contains(logText, secret) {
			t.Fatalf("access log leaks %q:\n%s", secret, truncate(logText, 1000))
		}
	}
}

// TestDisabledUserSessionsAndTokensRevoked pins PLAN 1397: disabling a user
// immediately invalidates their web session and API tokens.
func TestDisabledUserSessionsAndTokensRevoked(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t, env.client, "victim@example.com", "password123", "")
	csrf := env.csrf(t, env.client)
	victim, err := env.store.UserByEmail(context.Background(), "victim@example.com")
	if err != nil {
		t.Fatal(err)
	}

	// Token works before disabling.
	payload := strings.NewReader(`{"name":"t","scopes":"upload,read"}`)
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/api/v1/tokens", payload)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CSRF-Token", csrf)
	tokResp, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var tokenBody struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(tokResp.Body).Decode(&tokenBody)
	tokResp.Body.Close()
	if tokenBody.Token == "" {
		t.Fatal("token creation failed")
	}
	meReq, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/api/v1/me", nil)
	meReq.Header.Set("Authorization", "Bearer "+tokenBody.Token)
	meResp, err := env.client.Do(meReq)
	if err != nil {
		t.Fatal(err)
	}
	meResp.Body.Close()
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("bearer /me before disable = %d, want 200", meResp.StatusCode)
	}

	// Admin disables the user.
	hash, err := auth.HashPassword("admin-password-123")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	admin := &model.User{
		ID: id.New("usr"), Email: "admin2@example.com", PasswordHash: hash,
		Role: "admin", Status: "active", QuotaBytes: 10 << 20, CreatedAt: now, UpdatedAt: now,
	}
	if err := env.store.CreateUser(context.Background(), admin); err != nil {
		t.Fatal(err)
	}
	adminClient := env.newClient()
	env.login(t, adminClient, "admin2@example.com", "admin-password-123")
	csrfAdmin := env.csrf(t, adminClient)
	disResp, err := adminClient.PostForm(env.ts.URL+"/admin/users/"+victim.ID+"/status", url.Values{
		"csrf": {csrfAdmin}, "status": {"disabled"},
	})
	if err != nil {
		t.Fatal(err)
	}
	disResp.Body.Close()

	// Bearer token is unusable now.
	revReq, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/api/v1/me", nil)
	revReq.Header.Set("Authorization", "Bearer "+tokenBody.Token)
	revResp, err := env.newClient().Do(revReq)
	if err != nil {
		t.Fatal(err)
	}
	revResp.Body.Close()
	if revResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bearer /me after disable = %d, want 401", revResp.StatusCode)
	}

	// Cookie session is revoked: dashboard redirects to login.
	dashResp, err := env.client.Get(env.ts.URL + "/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(dashResp.Body)
	dashResp.Body.Close()
	if strings.Contains(string(body), "图库") {
		t.Fatal("disabled user still reaches the dashboard")
	}

	// Even the audit of the action is recorded.
	audits, err := env.store.ListAudit(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range audits {
		if a.Action == "admin.user_status" && a.Target == victim.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("audit rows %v missing admin.user_status for %s", audits, victim.ID)
	}
}

// TestAdminQuotaAndInvite covers PLAN 1395/1396 admin levers: quota
// adjustment and invite creation, with audit entries for both.
func TestAdminQuotaAndInvite(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t, env.client, "quota.user@example.com", "password123", "")
	victim, err := env.store.UserByEmail(context.Background(), "quota.user@example.com")
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := auth.HashPassword("admin-password-123")
	now := time.Now().Unix()
	admin := &model.User{
		ID: id.New("usr"), Email: "admin3@example.com", PasswordHash: hash,
		Role: "admin", Status: "active", QuotaBytes: 10 << 20, CreatedAt: now, UpdatedAt: now,
	}
	if err := env.store.CreateUser(context.Background(), admin); err != nil {
		t.Fatal(err)
	}
	adminClient := env.newClient()
	env.login(t, adminClient, "admin3@example.com", "admin-password-123")

	csrf := env.csrf(t, adminClient)
	quotaResp, err := adminClient.PostForm(env.ts.URL+"/admin/users/"+victim.ID+"/quota", url.Values{
		"csrf": {csrf}, "quota_mb": {"64"},
	})
	if err != nil {
		t.Fatal(err)
	}
	quotaResp.Body.Close()
	got, err := env.store.UserByID(context.Background(), victim.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.QuotaBytes != 64<<20 {
		t.Fatalf("quota_bytes = %d, want %d", got.QuotaBytes, int64(64<<20))
	}

	csrf = env.csrf(t, adminClient)
	invResp, err := adminClient.PostForm(env.ts.URL+"/admin/invites", url.Values{
		"csrf": {csrf}, "days": {"7"},
	})
	if err != nil {
		t.Fatal(err)
	}
	invBody, _ := io.ReadAll(invResp.Body)
	invResp.Body.Close()
	if invResp.StatusCode != http.StatusOK || !strings.Contains(string(invBody), "邀请码") {
		t.Fatalf("create invite status = %d, want 200 admin page with code", invResp.StatusCode)
	}
	invites, err := env.store.ListInvites(context.Background(), 10)
	if err != nil || len(invites) == 0 {
		t.Fatalf("invites = %v, %v", invites, err)
	}

	audits, err := env.store.ListAudit(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	var quotaAudited, inviteAudited bool
	for _, a := range audits {
		if a.Action == "admin.user_quota" {
			quotaAudited = true
		}
		if a.Action == "admin.invite_create" {
			inviteAudited = true
		}
	}
	if !quotaAudited || !inviteAudited {
		t.Fatalf("audit missing quota/invite entries: %v", audits)
	}
}

// TestGalleryPaginationAndQuotaDisplay covers PLAN 1387: the API paginates
// by cursor without duplicates, and the dashboard shows live quota usage.
func TestGalleryPaginationAndQuotaDisplay(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t, env.client, "pg@example.com", "password123", "")
	csrf := env.csrf(t, env.client)

	ids := map[string]bool{}
	var totalUsed int64
	for i := 1; i <= 5; i++ {
		resp, up := env.upload(t, env.client, makePNG(t, i, i, true), fmt.Sprintf("pg%d.png", i), csrf, "")
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("upload %d: status = %d", i, resp.StatusCode)
		}
		if ids[up.ID] {
			t.Fatalf("duplicate id %s across uploads", up.ID)
		}
		ids[up.ID] = true
		totalUsed += up.Size
	}

	dec := json.NewDecoder
	page := func(cursor string) ([]map[string]any, string) {
		href := env.ts.URL + "/api/v1/images?limit=2"
		if cursor != "" {
			href += "&cursor=" + cursor
		}
		resp, err := env.client.Get(href)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("list status = %d", resp.StatusCode)
		}
		var out struct {
			Items      []map[string]any `json:"items"`
			NextCursor string           `json:"next_cursor"`
		}
		if err := dec(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out.Items, out.NextCursor
	}

	seen := map[string]bool{}
	count := 0
	for cursor, final := "", false; !final; {
		items, next := page(cursor)
		for _, it := range items {
			id, _ := it["id"].(string)
			if seen[id] {
				t.Fatalf("id %s seen twice across pages", id)
			}
			seen[id] = true
			count++
		}
		if next == "" {
			final = true
		} else {
			cursor = next
		}
	}
	if count != 5 || len(seen) != 5 {
		t.Fatalf("paginated %d items, want 5 distinct", count)
	}

	// Dashboard quota line: used / quota with the app's own formatting.
	dashResp, err := env.client.Get(env.ts.URL + "/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	dashBody, _ := io.ReadAll(dashResp.Body)
	dashResp.Body.Close()
	// Test quota is 10 MiB (DefaultQuotaBytes); totalUsed < 1 KiB.
	want := fmt.Sprintf("%d B / 10.0 MiB", totalUsed)
	if !strings.Contains(string(dashBody), want) {
		t.Fatalf("dashboard missing quota %q; want line containing %q", want, truncate(string(dashBody), 300))
	}
}

// TestPublicContactFooter covers PLAN 1226: a public abuse contact is
// shown in footers when CONTACT_EMAIL is configured, hidden when empty.
func TestPublicContactFooter(t *testing.T) {
	bare := newTestEnv(t, nil)
	resp, err := http.Get(bare.ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(body), "mailto:") {
		t.Fatal("default login footer must not contain a contact link")
	}

	with := newTestEnv(t, func(c *config.Config) { c.PublicContact = "abuse@example.com" })
	resp, err = http.Get(with.ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `mailto:abuse@example.com`) ||
		!strings.Contains(string(body), "滥用反馈") {
		t.Fatalf("configured footer missing contact link:\n%s", truncate(string(body), 400))
	}
}

// TestAuditTrailPersistence exercises the mutation audit writes end to end:
// register, upload, delete, token create/revoke and password change must
// each leave an actor/action/target record.
func TestAuditTrailPersistence(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t, env.client, "audit@example.com", "password123", "")
	csrf := env.csrf(t, env.client)

	_, up := env.upload(t, env.client, makePNG(t, 2, 2, false), "a.png", csrf, "")
	if up.ID == "" {
		t.Fatal("upload failed")
	}
	delReq, _ := http.NewRequest(http.MethodDelete, env.ts.URL+"/api/v1/images/"+up.ID, nil)
	delReq.Header.Set("X-CSRF-Token", csrf)
	delResp, err := env.client.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", delResp.StatusCode)
	}

	payload := strings.NewReader(`{"name":"t","scopes":"upload,read,delete"}`)
	tokReq, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/api/v1/tokens", payload)
	tokReq.Header.Set("Content-Type", "application/json")
	tokReq.Header.Set("X-CSRF-Token", csrf)
	tokResp, err := env.client.Do(tokReq)
	if err != nil {
		t.Fatal(err)
	}
	var tokBody struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(tokResp.Body).Decode(&tokBody)
	tokResp.Body.Close()
	revReq, _ := http.NewRequest(http.MethodDelete, env.ts.URL+"/api/v1/tokens/"+tokBody.ID, nil)
	revReq.Header.Set("X-CSRF-Token", csrf)
	revResp, err := env.client.Do(revReq)
	if err != nil {
		t.Fatal(err)
	}
	revResp.Body.Close()

	csrf = env.csrf(t, env.client)
	pwResp, err := env.client.PostForm(env.ts.URL+"/settings/password", url.Values{
		"csrf": {csrf}, "current": {"password123"}, "new": {"new-password-456"}, "confirm": {"new-password-456"},
	})
	if err != nil {
		t.Fatal(err)
	}
	pwResp.Body.Close()

	audits, err := env.store.ListAudit(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	actions := map[string]bool{}
	for _, a := range audits {
		actions[a.Action] = true
		if a.CreatedAt <= 0 {
			t.Errorf("audit %s has no timestamp", a.Action)
		}
	}
	for _, want := range []string{"user.register", "image.delete", "token.create", "token.revoke", "user.password_change"} {
		if !actions[want] {
			t.Errorf("missing audit action %q (have %v)", want, actions)
		}
	}
}

// TestUploadStaticWiring pins PLAN 1386's no-browser-DOM surface: the
// dashboard must expose the drag/drop/paste elements and script hooks, and
// the served script must wire them to the API upload endpoint.
// TestWebUploadCleansMultipartTempFiles pins PERF-02: the web upload
// handler removes the Go multipart spill files it creates on disk.
// TestWebUploadRateLimit pins BUS-03: the browser upload endpoint shares
// the per-user 60/minute bucket with the API endpoint.
func TestWebUploadRateLimit(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t, env.client, "web-rl@example.com", "password123", "")
	csrf := env.csrf(t, env.client)
	data := makePNG(t, 1, 1, false)

	post := func() (*http.Response, error) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		if err := mw.WriteField("csrf", csrf); err != nil {
			return nil, err
		}
		part, err := mw.CreateFormFile("file", "tiny.png")
		if err != nil {
			return nil, err
		}
		if _, err := part.Write(data); err != nil {
			return nil, err
		}
		if err := mw.Close(); err != nil {
			return nil, err
		}
		req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/upload", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		return env.client.Do(req)
	}

	var last *http.Response
	for i := 0; i < 61; i++ {
		resp, err := post()
		if err != nil {
			t.Fatal(err)
		}
		last = resp
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if i < 60 && resp.StatusCode != http.StatusOK {
			t.Fatalf("web upload %d: status = %d, want 200 (%s)", i+1, resp.StatusCode, truncate(string(body), 200))
		}
	}
	if last.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("61st web upload status = %d, want 429", last.StatusCode)
	}
	if got := last.Header.Get("Retry-After"); got == "" {
		t.Fatal("web upload 429 missing Retry-After")
	}
}

func TestWebUploadCleansMultipartTempFiles(t *testing.T) {
	env := newTestEnv(t, func(c *config.Config) { c.MaxUploadBytes = 8 << 20 })
	env.register(t, env.client, "multipart@example.com", "password123", "")
	csrf := env.csrf(t, env.client)

	snapshot := func() map[string]bool {
		t.Helper()
		entries, err := os.ReadDir(os.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "multipart-") {
				out[entry.Name()] = true
			}
		}
		return out
	}
	before := snapshot()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("csrf", csrf); err != nil {
		t.Fatal(err)
	}
	fileWriter, err := mw.CreateFormFile("file", "big.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fileWriter.Write(makePNG(t, 1200, 1200, true)); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "上传成功") {
		t.Fatalf("web upload status=%d body=%s", resp.StatusCode, truncate(string(body), 300))
	}

	after := snapshot()
	for name := range after {
		if !before[name] {
			t.Fatalf("multipart temp file not removed: %s", name)
		}
	}
}

func TestUploadStaticWiring(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t, env.client, "static@example.com", "password123", "")

	dashResp, err := env.client.Get(env.ts.URL + "/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	dashBody, _ := io.ReadAll(dashResp.Body)
	dashResp.Body.Close()
	if !strings.Contains(string(dashBody), `id="upload-form"`) ||
		!strings.Contains(string(dashBody), `id="dropzone"`) ||
		!strings.Contains(string(dashBody), `id="file-input"`) ||
		!strings.Contains(string(dashBody), `type="file"`) ||
		!strings.Contains(string(dashBody), `name="file"`) ||
		!strings.Contains(string(dashBody), `multiple`) ||
		!strings.Contains(string(dashBody), "/static/app.js") {
		t.Fatal("dashboard is missing drag/paste upload wiring elements")
	}

	jsResp, err := http.Get(env.ts.URL + "/static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	jsBody, _ := io.ReadAll(jsResp.Body)
	jsResp.Body.Close()
	js := string(jsBody)
	for _, want := range []string{
		`getElementById("dropzone")`,
		`getElementById("file-input")`,
		`"dragenter"`,
		`"dragover"`,
		`"dragleave"`,
		`"drop"`,
		`e.dataTransfer.files`,
		`document.addEventListener("paste"`,
		`e.clipboardData?.files`,
		`uploadFiles(input.files)`,
		`xhr.open("POST", "/api/v1/upload")`,
		`navigator.clipboard.writeText`,
	} {
		if !strings.Contains(js, want) {
			t.Fatalf("static app.js missing %q", want)
		}
	}
}

// TestOpenAPIAndVerifyScriptMatchRoutes guards the public API contract:
// every documented operation and every operation invoked by
// scripts/verify-cdn.sh must correspond to an operation registered in
// server.go, and every API/origin route registered there must be documented.
func TestOpenAPIAndVerifyScriptMatchRoutes(t *testing.T) {
	readRepoFile := func(name string) string {
		t.Helper()
		path := filepath.Join("..", "..", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(data)
	}

	// Actual API/origin routes registered in server.go.
	serverSource := readRepoFile(filepath.Join("internal", "server", "server.go"))
	actualOps := map[string]struct{}{}
	routeRe := regexp.MustCompile(`mux\.(?:HandleFunc|Handle)\("([A-Z]+)\s+([^"]+)"`)
	for _, match := range routeRe.FindAllStringSubmatch(serverSource, -1) {
		path := strings.ReplaceAll(match[2], "{key...}", "{key}")
		if strings.HasPrefix(path, "/api/") || path == "/i/{key}" || path == "/healthz" || path == "/readyz" {
			actualOps[match[1]+" "+path] = struct{}{}
		}
	}
	if len(actualOps) == 0 {
		t.Fatal("no API routes parsed from internal/server/server.go")
	}

	routeSupported := func(op string) bool {
		if _, ok := actualOps[op]; ok {
			return true
		}
		parts := strings.SplitN(op, " ", 2)
		if len(parts) != 2 {
			return false
		}
		// http.ServeMux automatically supports HEAD on GET patterns.
		_, getExists := actualOps["GET "+parts[1]]
		return parts[0] == "HEAD" && getExists
	}

	// OpenAPI operations.
	openAPI := readRepoFile(filepath.Join("docs", "openapi.yaml"))
	documentedOps := map[string]struct{}{}
	inPaths := false
	currentPath := ""
	pathLineRe := regexp.MustCompile(`^  (/[^\s:]+):$`)
	methodLineRe := regexp.MustCompile(`^    (get|post|put|patch|delete|head|options):$`)
	for _, line := range strings.Split(openAPI, "\n") {
		if strings.HasPrefix(line, "paths:") {
			inPaths = true
			continue
		}
		if inPaths && line != "" && !strings.HasPrefix(line, " ") {
			inPaths = false
			continue
		}
		if !inPaths {
			continue
		}
		if match := pathLineRe.FindStringSubmatch(line); match != nil {
			currentPath = match[1]
			continue
		}
		if match := methodLineRe.FindStringSubmatch(line); match != nil && currentPath != "" {
			documentedOps[strings.ToUpper(match[1])+" "+currentPath] = struct{}{}
		}
	}
	if len(documentedOps) == 0 {
		t.Fatal("no OpenAPI operations parsed from docs/openapi.yaml")
	}
	for op := range documentedOps {
		if !routeSupported(op) {
			t.Errorf("documented OpenAPI route is not registered: %s", op)
		}
	}
	for op := range actualOps {
		if _, ok := documentedOps[op]; !ok {
			t.Errorf("registered route is missing from OpenAPI: %s", op)
		}
	}

	// Endpoints invoked by scripts/verify-cdn.sh.
	script := readRepoFile(filepath.Join("scripts", "verify-cdn.sh"))
	normalizeScriptPath := func(rawPath string) string {
		path := strings.ReplaceAll(rawPath, "/$image_id", "/{id}")
		return strings.ReplaceAll(path, "{image_id}", "{id}")
	}
	scriptOps := map[string]struct{}{}
	scriptPaths := map[string]struct{}{}
	scriptExplicitRe := regexp.MustCompile(`-X\s+(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\s+"\$PIC_BASE(/[^"\s]+)"`)
	for _, match := range scriptExplicitRe.FindAllStringSubmatch(script, -1) {
		path := normalizeScriptPath(match[2])
		scriptOps[match[1]+" "+path] = struct{}{}
		scriptPaths[path] = struct{}{}
	}
	scriptPlainRe := regexp.MustCompile(`"\$PIC_BASE(/[^"\s]+)"`)
	for _, match := range scriptPlainRe.FindAllStringSubmatch(script, -1) {
		path := normalizeScriptPath(match[1])
		if _, exists := scriptPaths[path]; exists {
			continue
		}
		scriptOps["GET "+path] = struct{}{}
		scriptPaths[path] = struct{}{}
	}
	// The script follows the returned image URL (an /i/{key} origin path);
	// it also uses codeless `$IMG_BASE` only as a stripped input, so those
	// dynamic image calls are represented with their explicit methods.
	if strings.Contains(script, `image_url=$(python3`) && strings.Contains(script, `"$image_url"`) {
		scriptOps["GET /i/{key}"] = struct{}{}
		scriptOps["HEAD /i/{key}"] = struct{}{}
	}
	for op := range scriptOps {
		if !routeSupported(op) {
			t.Errorf("verify-cdn.sh endpoint is not registered: %s", op)
		}
		if _, ok := documentedOps[op]; !ok {
			t.Errorf("verify-cdn.sh endpoint is not documented: %s", op)
		}
	}
}

// TestSettingsTokenDisplayOnceAndRevoke covers PLAN 1388: the settings page
// creates and revokes web-session API tokens, and the plaintext appears
// only in the immediate creation response.
func TestSettingsTokenDisplayOnceAndRevoke(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t, env.client, "settings-token@example.com", "password123", "")
	csrf := env.csrf(t, env.client)

	createResp, err := env.client.PostForm(env.ts.URL+"/settings/tokens", url.Values{
		"csrf": {csrf}, "name": {"web-token-e2e"}, "scopes": {"upload,read"},
	})
	if err != nil {
		t.Fatalf("create web token: %v", err)
	}
	createBody, _ := io.ReadAll(createResp.Body)
	createResp.Body.Close()
	if createResp.StatusCode != http.StatusOK || !strings.Contains(string(createBody), "新 Token（只显示这一次）") {
		t.Fatalf("create web token response = %d, body=%s", createResp.StatusCode, truncate(string(createBody), 300))
	}
	tokenRe := regexp.MustCompile(`<code class="token">([^<]+)</code>`)
	plaintext := tokenRe.FindStringSubmatch(string(createBody))[1]
	if !strings.HasPrefix(plaintext, "fp_") {
		t.Fatalf("plaintext token = %q, want fp_ prefix", plaintext)
	}

	user, err := env.store.UserByEmail(context.Background(), "settings-token@example.com")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := env.store.ListTokens(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored tokens = %d, want 1", len(stored))
	}
	storedToken := stored[0]
	if storedToken.TokenHash == plaintext || storedToken.TokenPrefix == plaintext {
		t.Fatal("full plaintext token was persisted")
	}

	settingsResp, err := env.client.Get(env.ts.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	settingsBody, _ := io.ReadAll(settingsResp.Body)
	settingsResp.Body.Close()
	settingsText := string(settingsBody)
	if strings.Contains(settingsText, plaintext) {
		t.Fatal("settings page exposes the full token after creation")
	}
	if !strings.Contains(settingsText, storedToken.TokenPrefix) {
		t.Fatalf("settings page does not show the token prefix %q", storedToken.TokenPrefix)
	}

	listReq, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/api/v1/tokens", nil)
	listResp, err := env.client.Do(listReq)
	if err != nil {
		t.Fatal(err)
	}
	listBody, _ := io.ReadAll(listResp.Body)
	listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK || strings.Contains(string(listBody), plaintext) {
		t.Fatalf("token list status=%d leaks plaintext=%v", listResp.StatusCode, strings.Contains(string(listBody), plaintext))
	}

	// The created token works until the settings page revokes it.
	meReq, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/api/v1/me", nil)
	meReq.Header.Set("Authorization", "Bearer "+plaintext)
	meResp, err := env.newClient().Do(meReq)
	if err != nil {
		t.Fatal(err)
	}
	meResp.Body.Close()
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("bearer /me = %d, want 200", meResp.StatusCode)
	}

	csrf = env.csrf(t, env.client)
	deleteResp, err := env.client.PostForm(env.ts.URL+"/settings/tokens/"+storedToken.ID+"/delete", url.Values{"csrf": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	deleteResp.Body.Close()

	remaining, err := env.store.ListTokens(context.Background(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("tokens after web revoke = %d, want 0", len(remaining))
	}
	revokedReq, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/api/v1/me", nil)
	revokedReq.Header.Set("Authorization", "Bearer "+plaintext)
	revokedResp, err := env.newClient().Do(revokedReq)
	if err != nil {
		t.Fatal(err)
	}
	revokedResp.Body.Close()
	if revokedResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked bearer /me = %d, want 401", revokedResp.StatusCode)
	}
}

func TestCustomBucketSettingsUploadAndDelete(t *testing.T) {
	env := newTestEnv(t, nil)
	env.register(t, env.client, "buckets@example.com", "password123", "")
	csrf := env.csrf(t, env.client)
	ctx := context.Background()

	resp, err := env.client.PostForm(env.ts.URL+"/settings/buckets", url.Values{
		"csrf":              {csrf},
		"name":              {"OCI Tokyo"},
		"region":            {"ap-tokyo-1"},
		"bucket":            {"fujipic-test"},
		"access_key_id":     {"AKIAEXAMPLE"},
		"secret_access_key": {"super-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "OCI Tokyo") {
		t.Fatalf("add bucket status=%d body=%s", resp.StatusCode, truncate(string(body), 400))
	}
	user, err := env.store.UserByEmail(ctx, "buckets@example.com")
	if err != nil {
		t.Fatal(err)
	}
	list, err := env.store.ListStorageBuckets(ctx, user.ID)
	if err != nil || len(list) != 1 {
		t.Fatalf("stored buckets = %+v, %v", list, err)
	}
	b := list[0]
	custom := env.customObjects[b.ID]
	if custom == nil {
		t.Fatal("bucket factory was not called for the new bucket")
	}

	data := makePNG(t, 4, 4, false)
	upResp, up := env.uploadToBucket(t, env.client, data, "custom.png", csrf, b.ID)
	if upResp.StatusCode != http.StatusCreated {
		t.Fatalf("bucket upload status = %d", upResp.StatusCode)
	}
	if up.BucketID != b.ID {
		t.Fatalf("upload bucket_id = %q, want %q", up.BucketID, b.ID)
	}
	if env.objects.Len() != 0 || custom.Len() != 1 {
		t.Fatalf("objects default=%d custom=%d, want 0 and 1", env.objects.Len(), custom.Len())
	}

	getResp, err := http.Get(up.URL)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK || !bytes.Equal(got, data) {
		t.Fatalf("custom bucket image GET status=%d", getResp.StatusCode)
	}

	delReq, _ := http.NewRequest(http.MethodDelete, env.ts.URL+"/api/v1/images/"+up.ID, nil)
	delReq.Header.Set("X-CSRF-Token", csrf)
	delResp, err := env.client.Do(delReq)
	if err != nil {
		t.Fatal(err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete image status = %d", delResp.StatusCode)
	}
	if custom.Len() != 0 {
		t.Fatalf("custom bucket still has %d objects after delete", custom.Len())
	}

	resp, err = env.client.PostForm(env.ts.URL+"/settings/buckets/"+b.ID+"/delete", url.Values{"csrf": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if _, err := env.store.StorageBucketByID(ctx, b.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("bucket still exists after delete: %v", err)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
