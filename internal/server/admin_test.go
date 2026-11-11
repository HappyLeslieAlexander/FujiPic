package server_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"fujipic/internal/auth"
	"fujipic/internal/config"
	"fujipic/internal/id"
	"fujipic/internal/model"
	"fujipic/internal/store"
	"fujipic/internal/upload"
)

func adminSession(t *testing.T, env *testEnv) (*model.User, *http.Client) {
	t.Helper()
	hash, err := auth.HashPassword("admin-password-123")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	user := &model.User{ID: id.New("usr"), Email: "admin@example.test", PasswordHash: hash,
		Role: "admin", Status: "active", QuotaBytes: 10 << 20, CreatedAt: now, UpdatedAt: now}
	if err := env.store.CreateUser(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	client := env.newClient()
	env.login(t, client, user.Email, "admin-password-123")
	return user, client
}

func TestAdminCreatesOrdinaryUser(t *testing.T) {
	env := newTestEnv(t, func(c *config.Config) { c.AllowOpenRegistration = false })
	admin, client := adminSession(t, env)
	csrf := env.csrf(t, client)
	form := url.Values{"csrf": {csrf}, "email": {"NewUser@Example.Test"}, "password": {"new-user-password-123"}, "role": {"admin"}}
	resp, err := client.PostForm(env.ts.URL+"/admin/users", form)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "普通用户已创建") || strings.Contains(string(body), form.Get("password")) {
		t.Fatal("admin user creation failed or exposed password")
	}
	user, err := env.store.UserByEmail(context.Background(), "newuser@example.test")
	if err != nil {
		t.Fatal(err)
	}
	if user.Role != "user" || user.Status != "active" || user.QuotaBytes != env.cfg.DefaultQuotaBytes {
		t.Fatal("created user did not receive ordinary-user defaults")
	}
	if ok, err := auth.VerifyPassword(form.Get("password"), user.PasswordHash); err != nil || !ok {
		t.Fatal("created password did not verify")
	}
	ordinary := env.newClient()
	env.login(t, ordinary, user.Email, form.Get("password"))
	// Creation must not replace the admin's session with the new account.
	resp, err = client.Get(env.ts.URL + "/admin")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("admin lost their session")
	}
	for _, tc := range []struct{ email, password, csrf string }{
		{"newuser@example.test", "replacement-password", csrf},
		{"invalid", "long-enough-password", csrf},
		{"invalid-pass@example.test", "short", csrf},
		{"csrf@example.test", "long-enough-password", "wrong"},
	} {
		resp, err := client.PostForm(env.ts.URL+"/admin/users", url.Values{
			"csrf": {tc.csrf}, "email": {tc.email}, "password": {tc.password},
		})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	count, _ := env.store.CountUsers(context.Background())
	if count != 2 {
		t.Fatalf("invalid create changed user count to %d", count)
	}
	logs, _ := env.store.ListAudit(context.Background(), 20)
	found := false
	for _, log := range logs {
		if log.Action == "admin.user_create" && log.ActorID != nil && *log.ActorID == admin.ID && log.Target == user.ID {
			found = true
		}
		if strings.Contains(log.Meta, form.Get("password")) {
			t.Fatal("audit exposed password")
		}
	}
	if !found {
		t.Fatal("missing user creation audit")
	}
	for _, path := range []string{"/admin/users", "/admin/images/not-found/delete"} {
		resp, err := ordinary.PostForm(env.ts.URL+path, url.Values{"csrf": {env.csrf(t, ordinary)}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Fatalf("ordinary user POST %s = %d", path, resp.StatusCode)
		}
	}
}

func TestAdminViewsAndDeletesUserImages(t *testing.T) {
	env := newTestEnv(t, nil)
	admin, client := adminSession(t, env)
	ownerClient := env.newClient()
	env.register(t, ownerClient, "owner@example.test", "owner-password-123", "")
	ctx := context.Background()
	owner, _ := env.store.UserByEmail(ctx, "owner@example.test")
	resp, img := env.upload(t, ownerClient, makePNG(t, 2, 2, false), "owner-image.png", env.csrf(t, ownerClient), "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload = %d", resp.StatusCode)
	}
	resp, err := client.Get(env.ts.URL + "/admin/images")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "owner@example.test") || !strings.Contains(string(body), img.ID) || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("admin gallery missing user image, owner, or private cache policy")
	}
	// Existing owner-only API permissions must not broaden even for admins.
	if _, err := env.store.ImageByID(ctx, admin.ID, img.ID); err != store.ErrNotFound {
		t.Fatal("owner-only lookup exposed another user's image")
	}
	if err := env.uploads.DeleteAsAdmin(ctx, owner.ID, img.ID, "127.0.0.1"); err != upload.ErrAdminRequired {
		t.Fatal("non-admin service call deleted another user's image")
	}
	path := env.ts.URL + "/admin/images/" + img.ID + "/delete"
	for _, tc := range []struct {
		client *http.Client
		csrf   string
	}{
		{ownerClient, env.csrf(t, ownerClient)}, {client, "wrong"},
	} {
		resp, err := tc.client.PostForm(path, url.Values{"csrf": {tc.csrf}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Fatalf("unauthorized admin delete = %d", resp.StatusCode)
		}
	}
	csrf := env.csrf(t, client)
	for i := 0; i < 2; i++ {
		resp, err := client.PostForm(path, url.Values{"csrf": {csrf}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("admin delete = %d", resp.StatusCode)
		}
	}
	owner, _ = env.store.UserByID(ctx, owner.ID)
	if owner.UsedBytes != 0 || env.objects.Len() != 0 {
		t.Fatal("admin delete failed to release owner quota or remove object")
	}
	var purges int
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM cdn_purges WHERE url = ?`, img.URL).Scan(&purges); err != nil || purges != 1 {
		t.Fatal("admin delete did not transactionally queue exactly one CDN purge")
	}
	logs, _ := env.store.ListAudit(ctx, 20)
	found := false
	for _, log := range logs {
		if log.Action == "admin.image_delete" && log.ActorID != nil && *log.ActorID == admin.ID && log.Target == img.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("admin deletion attributed to wrong actor")
	}
	resp, err = ownerClient.Get(env.ts.URL + "/admin/images")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal("ordinary user can view admin gallery")
	}
	// Offset pagination must expose older uploads too, not only the latest 60.
	for i := 0; i < 61; i++ {
		image := &model.Image{ID: fmt.Sprintf("img_page_%03d", i), UserID: owner.ID,
			ObjectKey: fmt.Sprintf("i/page-%d.png", i), OriginalName: fmt.Sprintf("page-%d.png", i),
			MIME: "image/png", SizeBytes: 1, Width: 1, Height: 1, SHA256: fmt.Sprint(i), Visibility: "public", CreatedAt: 1}
		if err := env.store.InsertImage(ctx, image); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ path, present, absent string }{
		{"/admin/images", "img_page_060", "img_page_000"},
		{"/admin/images?page=2", "img_page_000", "img_page_060"},
	} {
		resp, err := client.Get(env.ts.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(body), tc.present) || strings.Contains(string(body), tc.absent) {
			t.Fatal("admin gallery pagination overlapped or missed images")
		}
	}
}
