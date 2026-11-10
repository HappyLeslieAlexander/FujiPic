package storage

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

func TestMemoryStore(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	data := []byte("0123456789abcdef")
	if err := m.Put(ctx, "i/2026/11/key.png", bytes.NewReader(data), int64(len(data)), "image/png"); err != nil {
		t.Fatal(err)
	}
	if m.Len() != 1 || m.Puts() != 1 {
		t.Fatalf("len=%d puts=%d", m.Len(), m.Puts())
	}

	obj, err := m.Get(ctx, "i/2026/11/key.png", "")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(obj.Body)
	obj.Body.Close()
	if !bytes.Equal(body, data) || obj.Size != int64(len(data)) || obj.StatusCode != 200 {
		t.Fatalf("get = %+v body=%q", obj, body)
	}
	if obj.ContentType != "image/png" || obj.ETag == "" || obj.LastModified.IsZero() {
		t.Fatalf("metadata = %+v", obj)
	}

	obj, err = m.Get(ctx, "i/2026/11/key.png", "bytes=2-5")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(obj.Body)
	obj.Body.Close()
	if string(body) != "2345" || obj.StatusCode != 206 || obj.ContentRange != "bytes 2-5/16" {
		t.Fatalf("range = %+v body=%q", obj, body)
	}

	obj, err = m.Get(ctx, "i/2026/11/key.png", "bytes=-4")
	if err != nil {
		t.Fatal(err)
	}
	body, _ = io.ReadAll(obj.Body)
	obj.Body.Close()
	if string(body) != "cdef" {
		t.Fatalf("suffix range body = %q", body)
	}

	obj, err = m.Get(ctx, "i/2026/11/key.png", "bytes=99-100")
	if err != nil {
		t.Fatal(err)
	}
	obj.Body.Close()
	if obj.StatusCode != 416 || obj.ContentRange != "bytes */16" {
		t.Fatalf("invalid range = %+v", obj)
	}

	head, err := m.Head(ctx, "i/2026/11/key.png")
	if err != nil {
		t.Fatal(err)
	}
	head.Body.Close()
	if head.Size != int64(len(data)) {
		t.Fatalf("head size = %d", head.Size)
	}

	if err := m.Delete(ctx, "i/2026/11/key.png"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(ctx, "i/2026/11/key.png", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get deleted err = %v", err)
	}
	if _, err := m.Head(ctx, "i/2026/11/key.png"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("head deleted err = %v", err)
	}
	// Deleting a missing object is idempotent.
	if err := m.Delete(ctx, "missing"); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryStoreSizeMismatch(t *testing.T) {
	m := NewMemory()
	err := m.Put(context.Background(), "k", bytes.NewReader([]byte("abc")), 10, "image/png")
	if err == nil {
		t.Fatal("expected size mismatch error")
	}
}

func TestMemoryEmptyObjectRangeIsUnsatisfiable(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	if err := m.Put(ctx, "empty", bytes.NewReader(nil), 0, "image/png"); err != nil {
		t.Fatal(err)
	}
	for _, header := range []string{"bytes=-4", "bytes=0-", "bytes=0-0"} {
		obj, err := m.Get(ctx, "empty", header)
		if err != nil {
			t.Fatalf("range %q: %v", header, err)
		}
		obj.Body.Close()
		if obj.StatusCode != 416 || obj.ContentRange != "bytes */0" || obj.Size != 0 {
			t.Fatalf("range %q on empty object = %+v, want 416 bytes */0", header, obj)
		}
	}
}
