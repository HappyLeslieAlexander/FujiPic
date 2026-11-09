// Package imagefile validates uploaded images and streams them to a temp file
// while computing a SHA-256 hash.
package imagefile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"

	_ "golang.org/x/image/webp"
)

var (
	ErrTooLarge    = errors.New("image too large")
	ErrUnsupported = errors.New("unsupported image type")
	ErrDimensions  = errors.New("image dimensions exceed limits")
)

type Info struct {
	TempPath string
	Size     int64
	SHA256   string
	MIME     string
	Ext      string
	Width    int
	Height   int
}

var allowed = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// Process streams r into a temp file, enforces the size limit, sniffs the
// real type, and decodes only the image header for dimensions.
func Process(r io.Reader, tmpDir string, maxBytes, maxPixels int64, maxDimension int) (*Info, error) {
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	f, err := os.CreateTemp(tmpDir, "fujipic-upload-*")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	cleanup := func() {
		f.Close()
		os.Remove(f.Name())
	}

	hasher := sha256.New()
	limited := io.LimitReader(r, maxBytes+1)
	n, err := io.Copy(io.MultiWriter(f, hasher), limited)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("write temp file: %w", err)
	}
	if n > maxBytes {
		cleanup()
		return nil, ErrTooLarge
	}
	if n == 0 {
		cleanup()
		return nil, ErrUnsupported
	}
	if err := f.Chmod(0o600); err != nil {
		cleanup()
		return nil, err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, err
	}

	head := make([]byte, 512)
	hn, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		cleanup()
		return nil, err
	}
	head = head[:hn]
	mimeType := http.DetectContentType(head)
	if len(head) >= 12 && string(head[0:4]) == "RIFF" && string(head[8:12]) == "WEBP" {
		mimeType = "image/webp"
	}
	ext, ok := allowed[mimeType]
	if !ok {
		cleanup()
		return nil, ErrUnsupported
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, err
	}
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("%w: decode config: %v", ErrUnsupported, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		cleanup()
		return nil, ErrUnsupported
	}
	if cfg.Width > maxDimension || cfg.Height > maxDimension ||
		int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		cleanup()
		return nil, ErrDimensions
	}

	return &Info{
		TempPath: f.Name(),
		Size:     n,
		SHA256:   hex.EncodeToString(hasher.Sum(nil)),
		MIME:     mimeType,
		Ext:      ext,
		Width:    cfg.Width,
		Height:   cfg.Height,
	}, nil
}

// Cleanup removes the temp file; safe to call multiple times.
func (i *Info) Cleanup() {
	if i != nil && i.TempPath != "" {
		_ = os.Remove(i.TempPath)
		i.TempPath = ""
	}
}

// ExtensionFor returns the canonical extension for an allowed MIME type.
func ExtensionFor(mimeType string) string { return allowed[mimeType] }

// BaseName returns a safe display name for an uploaded file. It never
// influences the object key.
func BaseName(name string) string {
	name = filepath.Base(name)
	if name == "." || name == "/" || name == "" {
		return "image"
	}
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}
