package server_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"fujipic/internal/imagefile"
)

type testInspector struct{}

func (testInspector) Inspect(_ context.Context, _, format string, _ int64, _ int) (imagefile.Metadata, error) {
	return imagefile.Metadata{Format: format, Width: 10, Height: 10}, nil
}

func TestExtendedOriginalAndPreviewDelivery(t *testing.T) {
	env := newTestEnv(t, nil)
	u, client := adminSession(t, env)
	env.uploads.Inspector = testInspector{}
	ctx := context.Background()
	for _, ext := range []string{"avif", "bmp", "tif", "svg", "raw", "nef", "dng", "cr2"} {
		res, err := env.uploads.Upload(ctx, u, strings.NewReader("validated-in-isolated-decoder-"+ext), "test."+ext, "public")
		if err != nil {
			t.Fatal(ext, err)
		}
		resp, err := client.Get(env.ts.URL + "/" + res.Image.ObjectKey)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("CDN-Cache-Control"); got != "public, max-age=31536000" {
			t.Fatalf("original CDN cache policy: %q", got)
		}
		if resp.StatusCode != 200 || resp.Header.Get("Content-Disposition") != "attachment" || resp.Header.Get("Content-Type") != "application/octet-stream" || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") || len(resp.Cookies()) != 0 || resp.Header.Get("CDN-Cache-Control") == "" {
			t.Fatalf("unsafe original %s: %d %v", ext, resp.StatusCode, resp.Header)
		}
		j, err := env.store.ClaimMedia(ctx)
		if err != nil {
			t.Fatal(err)
		}
		key := strings.TrimSuffix(res.Image.ObjectKey, "."+imagefile.ExtendedFormat(res.Image.ObjectKey)) + "-abcdefghijklmnopqrstuv.preview.png"
		png := makePNG(t, 2, 2, false)
		if err := env.store.ReservePreview(ctx, *j, key, int64(len(png))); err != nil {
			t.Fatal(err)
		}
		if err := env.objects.Put(ctx, key, strings.NewReader(string(png)), int64(len(png)), "image/png"); err != nil {
			t.Fatal(err)
		}
		resp, err = client.Get(env.ts.URL + "/" + key)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 || resp.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("unpublished preview cacheable")
		}
		if err := env.store.CompletePreview(ctx, *j); err != nil {
			t.Fatal(err)
		}
		req, _ := http.NewRequest("GET", env.ts.URL+"/"+key, nil)
		req.Header.Set("Range", "bytes=0-7")
		resp, err = client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if got := resp.Header.Get("CDN-Cache-Control"); got != "public, max-age=31536000" {
			t.Fatalf("preview CDN cache policy: %q", got)
		}
		if resp.StatusCode != 206 || len(body) != 8 || resp.Header.Get("Content-Type") != "image/png" || resp.Header.Get("Content-Disposition") != "inline" {
			t.Fatalf("preview range: %d %v", resp.StatusCode, resp.Header)
		}
		if err := env.uploads.Delete(ctx, u.ID, res.Image.ID, ""); err != nil {
			t.Fatal(err)
		}
		resp, err = client.Get(env.ts.URL + "/" + key)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatal("deleted preview visible")
		}
	}
}
