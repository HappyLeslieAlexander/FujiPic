package media

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"fujipic/internal/imagefile"
)

func TestIsolatedProtocolAndOutputValidation(t *testing.T) {
	dir, err := os.MkdirTemp("", "fp-media-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "s")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := png.Encode(&output, image.NewRGBA(image.Rect(0, 0, 4, 3))); err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Format") == "nef" {
			w.WriteHeader(415)
			return
		}
		if r.Header.Get("X-Format") == "raw" {
			w.Write([]byte("<svg/>"))
			return
		}
		if r.URL.Path == "/inspect" {
			w.Write([]byte(`{"format":"svg","width":4,"height":3}`))
			return
		}
		w.Write(output.Bytes())
	})}
	go srv.Serve(ln)
	defer srv.Close()
	path := filepath.Join(dir, "input")
	if err := os.WriteFile(path, []byte("svg"), 0600); err != nil {
		t.Fatal(err)
	}
	c := New(socket)
	ctx := context.Background()
	meta, err := c.Inspect(ctx, path, "svg", 1000, 100)
	if err != nil || meta.Width != 4 {
		t.Fatal(meta, err)
	}
	if _, err := c.Inspect(ctx, path, "nef", 1000, 100); !errors.Is(err, imagefile.ErrUnsupported) {
		t.Fatal(err)
	}
	if _, err := c.Convert(ctx, path, "raw", dir, 1000, 100); !errors.Is(err, imagefile.ErrUnsupported) {
		t.Fatal("non-PNG published", err)
	}
	info, err := c.Convert(ctx, path, "svg", dir, 1000, 100)
	if err != nil {
		t.Fatal(err)
	}
	defer info.Cleanup()
	if info.MIME != "image/png" || info.Width != 4 || info.Height != 3 {
		t.Fatal(info)
	}
}
