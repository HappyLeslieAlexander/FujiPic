package storage

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Memory is an in-memory Store used by unit tests and local demos.
type Memory struct {
	mu      sync.RWMutex
	objects map[string]*memoryObject
	puts    int
	gets    int
}

type memoryObject struct {
	data        []byte
	contentType string
	etag        string
	modified    time.Time
}

func NewMemory() *Memory {
	return &Memory{objects: make(map[string]*memoryObject)}
}

func (m *Memory) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	data, err := io.ReadAll(io.LimitReader(body, size+1))
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return fmt.Errorf("size mismatch: got %d, want %d", len(data), size)
	}
	sum := md5.Sum(data)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = &memoryObject{
		data:        data,
		contentType: contentType,
		etag:        `"` + hex.EncodeToString(sum[:]) + `"`,
		modified:    time.Now().UTC(),
	}
	m.puts++
	return nil
}

func (m *Memory) Get(ctx context.Context, key, rangeHeader string) (*Object, error) {
	m.mu.RLock()
	obj, ok := m.objects[key]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrNotFound
	}
	m.mu.Lock()
	m.gets++
	m.mu.Unlock()

	size := int64(len(obj.data))
	out := &Object{
		Size:         size,
		ContentType:  obj.contentType,
		ETag:         obj.etag,
		LastModified: obj.modified,
		StatusCode:   200,
	}
	if rangeHeader == "" {
		out.Body = io.NopCloser(bytes.NewReader(obj.data))
		return out, nil
	}
	start, end, ok := parseRange(rangeHeader, size)
	if !ok {
		out.Body = io.NopCloser(bytes.NewReader(nil))
		out.StatusCode = 416
		out.Size = 0
		out.ContentRange = fmt.Sprintf("bytes */%d", size)
		return out, nil
	}
	out.Body = io.NopCloser(bytes.NewReader(obj.data[start : end+1]))
	out.Size = end - start + 1
	out.StatusCode = 206
	out.ContentRange = fmt.Sprintf("bytes %d-%d/%d", start, end, size)
	return out, nil
}

func (m *Memory) Head(ctx context.Context, key string) (*Object, error) {
	m.mu.RLock()
	obj, ok := m.objects[key]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrNotFound
	}
	return &Object{
		Body:         io.NopCloser(bytes.NewReader(nil)),
		Size:         int64(len(obj.data)),
		ContentType:  obj.contentType,
		ETag:         obj.etag,
		LastModified: obj.modified,
		StatusCode:   200,
	}, nil
}

func (m *Memory) Delete(ctx context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (m *Memory) Health(ctx context.Context) error { return nil }

func (m *Memory) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.objects)
}

func (m *Memory) Puts() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.puts
}

func (m *Memory) Gets() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.gets
}

// parseRange parses a single "bytes=start-end" range. Suffix ranges are
// supported; multiple ranges are rejected (416), which is acceptable for
// images and keeps the proxy simple.
func parseRange(header string, size int64) (int64, int64, bool) {
	if size <= 0 || !strings.HasPrefix(header, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(header, "bytes=")
	if strings.Contains(spec, ",") {
		return 0, 0, false
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	startStr, endStr := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	if startStr == "" {
		n, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true
	}
	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end := size - 1
	if endStr != "" {
		end, err = strconv.ParseInt(endStr, 10, 64)
		if err != nil || end < start {
			return 0, 0, false
		}
		if end >= size {
			end = size - 1
		}
	}
	return start, end, true
}
