package server_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"fujipic/internal/model"
)

func TestAdminImageFiltersAndBatchHTTP(t *testing.T) {
	env := newTestEnv(t, nil)
	admin, client := adminSession(t, env)
	ctx := context.Background()
	u := &model.User{ID: "usr_filter", Email: "filter@example.test", PasswordHash: "test", Role: "user", Status: "active", QuotaBytes: 10000, CreatedAt: 1, UpdatedAt: 1}
	if err := env.store.CreateUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	if err := env.store.CreateStorageBucket(ctx, &model.StorageBucket{ID: "bkt_test", Name: "Test bucket", Bucket: "test", Region: "test", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 62; i++ {
		owner, bid := u.ID, "bkt_test"
		if i == 61 {
			owner, bid = admin.ID, model.DefaultBucketID
		}
		img := &model.Image{ID: fmt.Sprintf("img_filter_%03d", i), UserID: owner, BucketID: bid, ObjectKey: fmt.Sprintf("i/filter/%03d.png", i), OriginalName: fmt.Sprintf("filter-%03d.png", i), MIME: "image/png", SHA256: fmt.Sprint(i), SizeBytes: 1, Width: 1, Height: 1, Visibility: "public", CreatedAt: int64(i + 1)}
		if err := env.store.InsertImage(ctx, img); err != nil {
			t.Fatal(err)
		}
	}
	get := func(path string, want int) string {
		t.Helper()
		resp, err := client.Get(env.ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("GET %s: %d %s", path, resp.StatusCode, body)
		}
		return string(body)
	}
	post := func(c *http.Client, values url.Values, want int) string {
		t.Helper()
		resp, err := c.PostForm(env.ts.URL+"/admin/images/delete", values)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != want {
			t.Fatalf("POST: %d %s", resp.StatusCode, body)
		}
		return string(body)
	}
	allButton := `name="mode" value="all"`
	if body := get("/admin/images", 200); strings.Contains(body, allButton) || !strings.Contains(body, `id="admin-match-count">62`) {
		t.Fatal("unfiltered controls/count")
	}
	q := url.Values{"user": {u.ID}, "bucket": {"bkt_test"}}
	body := get("/admin/images?"+q.Encode(), 200)
	if !strings.Contains(body, allButton) || !strings.Contains(body, `id="admin-match-count">61`) || strings.Count(body, `form="bulk-image-delete"`) != 60 {
		t.Fatal("filtered controls/count/page")
	}
	if !strings.Contains(body, `bucket=bkt_test&amp;page=2&amp;user=usr_filter`) {
		t.Fatal("pagination dropped filters")
	}
	q.Set("page", "2")
	if body := get("/admin/images?"+q.Encode(), 200); strings.Count(body, `form="bulk-image-delete"`) != 1 {
		t.Fatal("second page")
	}
	q = url.Values{"user": {u.ID, admin.ID}, "bucket": {"bkt_test", model.DefaultBucketID}}
	if strings.Contains(get("/admin/images?"+q.Encode(), 200), allButton) {
		t.Fatal("all options must normalize to no filter")
	}
	get("/admin/images?user=unknown", 400)
	get("/admin/images?bucket=unknown", 400)
	csrf := env.csrf(t, client)
	post(client, url.Values{"csrf": {csrf}, "mode": {"all"}, "fingerprint": {"x"}}, 400)
	post(client, url.Values{"csrf": {csrf}, "mode": {"selected"}}, 400)
	post(client, url.Values{"mode": {"selected"}, "image": {"img_filter_000"}}, 403)
	post(client, url.Values{"csrf": {csrf}, "mode": {"selected"}, "user": {admin.ID}, "image": {"img_filter_000"}}, 400)
	post(client, url.Values{"csrf": {csrf}, "mode": {"selected"}, "bucket": {"unknown"}, "image": {"img_filter_000"}}, 400)
	// Explicit selections work without filters, and duplicate IDs are deduplicated.
	post(client, url.Values{"csrf": {csrf}, "mode": {"selected"}, "image": {"img_filter_000", "img_filter_001", "img_filter_001"}}, 200)
	if n, _ := env.store.CountUserImages(ctx, u.ID); n != 59 {
		t.Fatalf("selected deletion: %d", n)
	}
	// The earlier fingerprint is now stale and must delete nothing.
	fp := regexp.MustCompile(`name="fingerprint" value="([^"]+)"`).FindStringSubmatch(body)[1]
	values := url.Values{"csrf": {csrf}, "mode": {"all"}, "user": {u.ID}, "fingerprint": {fp}}
	if result := post(client, values, 200); !strings.Contains(result, "筛选结果已变化") {
		t.Fatal("stale selection not explained")
	}
	if n, _ := env.store.CountUserImages(ctx, u.ID); n != 59 {
		t.Fatal("stale selection deleted images")
	}
	body = get("/admin/images?user="+u.ID, 200)
	values.Set("fingerprint", regexp.MustCompile(`name="fingerprint" value="([^"]+)"`).FindStringSubmatch(body)[1])
	post(client, values, 200)
	if n, _ := env.store.CountUserImages(ctx, u.ID); n != 0 {
		t.Fatal("filtered deletion incomplete")
	}
	if n, _ := env.store.CountUserImages(ctx, admin.ID); n != 1 {
		t.Fatal("deleted outside filter")
	}
	// Browser role checks still apply to the new route.
	regular := env.newClient()
	env.register(t, regular, "regular@example.test", "password123", "")
	post(regular, url.Values{"csrf": {env.csrf(t, regular)}, "mode": {"selected"}, "image": {"img_filter_061"}}, 403)
	// Header-based CSRF must still parse the form, and GET never mutates.
	get("/admin/images/delete", http.StatusMethodNotAllowed)
	if n, _ := env.store.CountUserImages(ctx, admin.ID); n != 1 {
		t.Fatal("GET mutated images")
	}
	req, _ := http.NewRequest("POST", env.ts.URL+"/admin/images/delete", strings.NewReader(url.Values{"mode": {"selected"}, "image": {"img_filter_061"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("header CSRF: %d", resp.StatusCode)
	}
	if n, _ := env.store.CountUserImages(ctx, admin.ID); n != 0 {
		t.Fatal("header CSRF did not parse selection")
	}
}
