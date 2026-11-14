package server_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"fujipic/internal/model"
)

func TestBatchPostDoesNotBroadenReviewedScope(t *testing.T) {
	env := newTestEnv(t, nil)
	_, client := adminSession(t, env)
	_, img := env.upload(t, client, makePNG(t, 1, 1, false), "test.png", env.csrf(t, client), "")
	// The reviewed page had two buckets. Removing the unused one leaves the
	// explicit default selection covering every option during POST validation.
	ctx := context.Background()
	if err := env.store.CreateStorageBucket(ctx, &model.StorageBucket{ID: "bkt_unused", Name: "unused", Bucket: "unused", Region: "test", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	resp, err := client.Get(env.ts.URL + "/admin/images?bucket=" + model.DefaultBucketID)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if err := env.store.DeleteStorageBucket(ctx, "bkt_unused"); err != nil {
		t.Fatal(err)
	}
	// Deterministically interleave a bucket change after option validation but
	// before selection, at the batch transaction's first write. A normalized
	// empty filter would incorrectly include the image in the new bucket.
	if _, err := env.db.Exec(`CREATE TRIGGER change_bucket_before_selection
		AFTER UPDATE OF updated_at ON users WHEN NEW.updated_at = OLD.updated_at
		BEGIN
		  INSERT OR IGNORE INTO storage_buckets(id,name,endpoint,region,bucket,access_key_id,secret_ciphertext,created_at,updated_at)
		  VALUES('bkt_new','new','','test','new','','',1,1);
		  UPDATE images SET bucket_id='bkt_new';
		END`); err != nil {
		t.Fatal(err)
	}
	values := url.Values{"csrf": {env.csrf(t, client)}, "mode": {"selected"}, "bucket": {model.DefaultBucketID}, "image": {img.ID}}
	resp, err = client.PostForm(env.ts.URL+"/admin/images/delete", values)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var deleted bool
	if err := env.db.QueryRow(`SELECT deleted_at IS NOT NULL FROM images WHERE id=?`, img.ID).Scan(&deleted); err != nil || deleted {
		t.Fatalf("image deleted=%v err=%v", deleted, err)
	}
}

func TestDashboardPaginationIsOwnerScoped(t *testing.T) {
	env := newTestEnv(t, nil)
	u, client := adminSession(t, env)
	ctx := context.Background()
	other := &model.User{ID: "usr_other", Email: "other@test", PasswordHash: "test", Role: "user", Status: "active", QuotaBytes: 1000, CreatedAt: 1, UpdatedAt: 1}
	if err := env.store.CreateUser(ctx, other); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 62; i++ {
		owner := u.ID
		if i == 61 {
			owner = other.ID
		}
		if err := env.store.InsertImage(ctx, &model.Image{ID: fmt.Sprintf("page_%03d", i), UserID: owner, ObjectKey: fmt.Sprintf("i/%03d.png", i), OriginalName: fmt.Sprintf("page-%03d.png", i), MIME: "image/png", SHA256: fmt.Sprint(i), SizeBytes: 1, Width: 1, Height: 1, Visibility: "public", CreatedAt: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		path          string
		status, count int
		link          string
	}{
		{"/dashboard", 200, 60, `href="/dashboard?page=2"`},
		{"/dashboard?page=2", 200, 1, `href="/dashboard?page=1"`},
		{"/dashboard?page=-1", 400, 0, ""},
	} {
		resp, err := client.Get(env.ts.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.status || strings.Count(string(body), `<figure class="thumb"`) != tc.count || !strings.Contains(string(body), tc.link) || strings.Contains(string(body), "page-061.png") {
			t.Fatalf("page %s status=%d", tc.path, resp.StatusCode)
		}
	}
}

func TestTokenJSONRejectsTrailingData(t *testing.T) {
	env := newTestEnv(t, nil)
	_, client := adminSession(t, env)
	csrf := env.csrf(t, client)
	for _, body := range []string{`{"name":"test"} {}`, `{"name":"test"} garbage`, `{"name":"test"}` + strings.Repeat(" ", 1<<20)} {
		req, _ := http.NewRequest("POST", env.ts.URL+"/api/v1/tokens", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 && resp.StatusCode != 413 {
			t.Fatalf("invalid JSON status=%d", resp.StatusCode)
		}
	}
	var count int
	if err := env.db.QueryRow(`SELECT COUNT(*) FROM api_tokens`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("tokens created: %d %v", count, err)
	}
}

func TestHeaderCSRFRejectsMalformedForm(t *testing.T) {
	env := newTestEnv(t, nil)
	_, client := adminSession(t, env)
	req, _ := http.NewRequest("POST", env.ts.URL+"/settings/tokens", strings.NewReader("name=test&scopes=read&broken=%zz"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", env.csrf(t, client))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatalf("partial form accepted: %d", resp.StatusCode)
	}
}
