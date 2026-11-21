package bucket

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"fujipic/internal/storage"
)

type probeStore struct {
	storage.Store
	body       string
	failDelete bool
}

func (s *probeStore) Get(ctx context.Context, key, rangeHeader string) (*storage.Object, error) {
	obj, err := s.Store.Get(ctx, key, rangeHeader)
	if err != nil {
		return nil, err
	}
	obj.Body.Close()
	obj.Body = io.NopCloser(strings.NewReader(s.body))
	return obj, nil
}

func (s *probeStore) Delete(ctx context.Context, key string) error {
	if s.failDelete {
		return errors.New("delete denied")
	}
	return s.Store.Delete(ctx, key)
}

func TestProbeVerifiesContentsAndDeletePermission(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		failDelete bool
	}{
		{"empty", "", false}, {"corrupt", "wrong", false},
		{"trailing", "fujipic-extra", false}, {"delete denied", "fujipic", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			objects := storage.NewMemory()
			if err := probe(context.Background(), &probeStore{Store: objects, body: tc.body, failDelete: tc.failDelete}); err == nil {
				t.Fatal("unusable bucket passed probe")
			}
			if !tc.failDelete && objects.Len() != 0 {
				t.Fatal("probe object leaked")
			}
		})
	}
}
