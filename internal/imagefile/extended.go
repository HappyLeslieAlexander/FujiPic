package imagefile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Inspector runs complex parsers outside the web process. The filename is only
// a routing hint: the isolated decoder must verify the actual format.
type Inspector interface {
	Inspect(context.Context, string, string, int64, int) (Metadata, error)
}

type Metadata struct {
	Format string `json:"format"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

var extendedTypes = map[string]string{
	"avif": "image/avif", "bmp": "image/bmp", "tiff": "image/tiff",
	"svg": "image/svg+xml", "raw": "application/x-camera-raw",
	"nef": "image/x-nikon-nef", "dng": "image/x-adobe-dng", "cr2": "image/x-canon-cr2",
}

func ExtendedFormat(filename string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(filename), "."))
	if ext == "tif" {
		ext = "tiff"
	}
	if _, ok := extendedTypes[ext]; ok {
		return ext
	}
	return ""
}

func NeedsPreview(mime string) bool {
	for _, value := range extendedTypes {
		if mime == value {
			return true
		}
	}
	return false
}

// ProcessExtended never decodes untrusted complex input in the Go server.
func ProcessExtended(ctx context.Context, r io.Reader, tmpDir, format string, maxBytes, maxPixels int64, maxDim int, inspector Inspector) (*Info, error) {
	if inspector == nil || extendedTypes[format] == "" {
		return nil, ErrUnsupported
	}
	if err := os.MkdirAll(tmpDir, 0700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp(tmpDir, "fujipic-upload-*")
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		f.Close()
		if !keep {
			os.Remove(f.Name())
		}
	}()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, maxBytes))
	if err != nil {
		return nil, err
	}
	var extra [1]byte
	if count, err := io.ReadFull(r, extra[:]); count > 0 {
		return nil, ErrTooLarge
	} else if err != nil && err != io.EOF {
		return nil, err
	}
	if n == 0 {
		return nil, ErrUnsupported
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	meta, err := inspector.Inspect(ctx, f.Name(), format, maxPixels, maxDim)
	if err != nil {
		return nil, err
	}
	if meta.Format != format {
		return nil, ErrUnsupported
	}
	if meta.Width <= 0 || meta.Height <= 0 || meta.Width > maxDim || meta.Height > maxDim || int64(meta.Width) > maxPixels/int64(meta.Height) {
		return nil, ErrDimensions
	}
	keep = true
	return &Info{TempPath: f.Name(), Size: n, SHA256: hex.EncodeToString(h.Sum(nil)), MIME: extendedTypes[format], Ext: "." + format, Width: meta.Width, Height: meta.Height}, nil
}
