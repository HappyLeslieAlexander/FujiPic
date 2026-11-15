package imagefile

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

type inspectorFunc func(context.Context, string, string, int64, int) (Metadata, error)

func (f inspectorFunc) Inspect(c context.Context, p, s string, n int64, d int) (Metadata, error) {
	return f(c, p, s, n, d)
}

func TestExtendedValidationAndCleanup(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		meta Metadata
		err  error
		size int64
		want error
	}{
		{"valid", Metadata{Format: "svg", Width: 10, Height: 10}, nil, 10, nil},
		{"oversize", Metadata{}, nil, 1, ErrTooLarge},
		{"wrong-format", Metadata{Format: "bmp", Width: 10, Height: 10}, nil, 10, ErrUnsupported},
		{"zero-height", Metadata{Format: "svg", Width: 10}, nil, 10, ErrDimensions},
		{"too-many-pixels", Metadata{Format: "svg", Width: 100, Height: 100}, nil, 10, ErrDimensions},
		{"decoder-rejected", Metadata{}, ErrUnsupported, 10, ErrUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			inspector := inspectorFunc(func(_ context.Context, path, format string, _ int64, _ int) (Metadata, error) {
				body, err := os.ReadFile(path)
				if err != nil || string(body) != "bytes" || format != "svg" {
					t.Fatal("wrong input", err)
				}
				return tc.meta, tc.err
			})
			info, err := ProcessExtended(ctx, strings.NewReader("bytes"), dir, "svg", tc.size, 1000, 1000, inspector)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
			if info != nil {
				if info.Size != 5 || info.MIME != "image/svg+xml" {
					t.Fatal(info)
				}
				info.Cleanup()
			}
			files, _ := os.ReadDir(dir)
			if len(files) != 0 {
				t.Fatal("temporary file leak")
			}
		})
	}
	if _, err := ProcessExtended(ctx, strings.NewReader("bytes"), t.TempDir(), "svg", 10, 1000, 1000, nil); !errors.Is(err, ErrUnsupported) {
		t.Fatal("unverified input accepted")
	}
}
