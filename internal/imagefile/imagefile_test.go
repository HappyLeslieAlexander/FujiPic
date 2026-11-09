package imagefile

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"testing"
)

func encodePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 9), uint8(y * 7), 128, 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestProcessPNG(t *testing.T) {
	data := encodePNG(t, 8, 6)
	info, err := Process(bytes.NewReader(data), t.TempDir(), 1<<20, 50_000_000, 12000)
	if err != nil {
		t.Fatal(err)
	}
	defer info.Cleanup()
	if info.MIME != "image/png" || info.Ext != ".png" || info.Width != 8 || info.Height != 6 {
		t.Fatalf("info = %+v", info)
	}
	if info.Size != int64(len(data)) {
		t.Fatalf("size = %d, want %d", info.Size, len(data))
	}
	if len(info.SHA256) != 64 {
		t.Fatalf("sha256 = %q", info.SHA256)
	}
	if _, err := os.Stat(info.TempPath); err != nil {
		t.Fatalf("temp file missing: %v", err)
	}
	path := info.TempPath
	info.Cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("temp file not removed: %v", err)
	}
}

func TestProcessJPEGAndGIF(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 5, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 5; x++ {
			img.Set(x, y, color.RGBA{200, 10, 10, 255})
		}
	}
	var jpegBuf bytes.Buffer
	if err := jpeg.Encode(&jpegBuf, img, nil); err != nil {
		t.Fatal(err)
	}
	info, err := Process(bytes.NewReader(jpegBuf.Bytes()), t.TempDir(), 1<<20, 50_000_000, 12000)
	if err != nil {
		t.Fatal(err)
	}
	info.Cleanup()
	if info.MIME != "image/jpeg" || info.Ext != ".jpg" {
		t.Fatalf("jpeg info = %+v", info)
	}

	var gifBuf bytes.Buffer
	if err := gif.Encode(&gifBuf, img, nil); err != nil {
		t.Fatal(err)
	}
	info, err = Process(bytes.NewReader(gifBuf.Bytes()), t.TempDir(), 1<<20, 50_000_000, 12000)
	if err != nil {
		t.Fatal(err)
	}
	info.Cleanup()
	if info.MIME != "image/gif" || info.Ext != ".gif" {
		t.Fatalf("gif info = %+v", info)
	}
}

func TestProcessRejectsUnsupported(t *testing.T) {
	for name, data := range map[string][]byte{
		"text": []byte("this is not an image"),
		"svg":  []byte(`<svg xmlns="http://www.w3.org/2000/svg"><rect width="10" height="10"/></svg>`),
		"html": []byte("<html><body>hi</body></html>"),
	} {
		_, err := Process(bytes.NewReader(data), t.TempDir(), 1<<20, 50_000_000, 12000)
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: err = %v, want ErrUnsupported", name, err)
		}
	}
}

func TestProcessLimits(t *testing.T) {
	data := encodePNG(t, 64, 64)
	if _, err := Process(bytes.NewReader(data), t.TempDir(), 10, 50_000_000, 12000); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("size limit err = %v", err)
	}
	if _, err := Process(bytes.NewReader(data), t.TempDir(), 1<<20, 100, 12000); !errors.Is(err, ErrDimensions) {
		t.Fatalf("pixel limit err = %v", err)
	}
	if _, err := Process(bytes.NewReader(data), t.TempDir(), 1<<20, 50_000_000, 10); !errors.Is(err, ErrDimensions) {
		t.Fatalf("dimension limit err = %v", err)
	}
}

func TestBaseName(t *testing.T) {
	if got := BaseName("../../etc/passwd"); got != "passwd" {
		t.Fatalf("BaseName traversal = %q", got)
	}
	if got := BaseName(""); got != "image" {
		t.Fatalf("BaseName empty = %q", got)
	}
	long := make([]byte, 300)
	for i := range long {
		long[i] = 'a'
	}
	if got := BaseName(string(long)); len(got) != 200 {
		t.Fatalf("BaseName long len = %d", len(got))
	}
}
