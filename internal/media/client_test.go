package media

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		if r.Header.Get("X-Format") == "bmp" {
			w.Header().Set("X-Media-Busy", "1")
			w.WriteHeader(503)
			return
		}
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
	if _, err := c.Inspect(ctx, path, "bmp", 1000, 100); !errors.Is(err, ErrBusy) {
		t.Fatal("busy not distinguished", err)
	}
	if err := c.gate.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Inspect(canceled, "does-not-exist", "svg", 1000, 100); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled waiter accepted", err)
	}
	c.gate.Release()
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type heldBody struct {
	io.Reader
	reading chan struct{}
	release chan struct{}
	started bool
}

func (b *heldBody) Read(p []byte) (int, error) {
	if !b.started {
		b.started = true
		close(b.reading)
		<-b.release
	}
	return b.Reader.Read(p)
}
func (*heldBody) Close() error { return nil }

func TestInspectionWaitsForConversionBodyAndHonorsCancellation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "input")
	if err := os.WriteFile(path, []byte("svg"), 0600); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := png.Encode(&output, image.NewRGBA(image.Rect(0, 0, 4, 3))); err != nil {
		t.Fatal(err)
	}
	body := &heldBody{Reader: bytes.NewReader(output.Bytes()), reading: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-body.release:
		default:
			close(body.release)
		}
	}()
	inspections := make(chan struct{}, 2)
	c := New("unused")
	c.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		defer r.Body.Close()
		var response io.ReadCloser = body
		if r.URL.Path == "/inspect" {
			inspections <- struct{}{}
			response = io.NopCloser(strings.NewReader(`{"format":"svg","width":4,"height":3}`))
		}
		return &http.Response{StatusCode: 200, Body: response}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	converted := make(chan error, 1)
	go func() {
		info, err := c.Convert(ctx, path, "svg", dir, 1000, 100)
		if info != nil {
			info.Cleanup()
		}
		converted <- err
	}()
	select {
	case <-body.reading:
	case <-ctx.Done():
		t.Fatal("conversion never started")
	}
	short, stop := context.WithTimeout(ctx, 30*time.Millisecond)
	defer stop()
	if _, err := c.Inspect(short, path, "svg", 1000, 100); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("waiting inspection was not canceled", err)
	}
	done := make(chan error, 1)
	go func() { _, err := c.Inspect(ctx, path, "svg", 1000, 100); done <- err }()
	select {
	case <-inspections:
		t.Fatal("inspection overlapped conversion response body")
	case err := <-done:
		t.Fatal("inspection rejected instead of waiting", err)
	case <-time.After(30 * time.Millisecond):
	}
	close(body.release)
	if err := <-converted; err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal("queued inspection failed", err)
	}
}
