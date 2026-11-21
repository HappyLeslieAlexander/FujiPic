// Package storage abstracts the private S3-compatible bucket.
package storage

import (
	"context"
	"errors"
	"io"
	"time"
)

var ErrNotFound = errors.New("object not found")

type Object struct {
	Body         io.ReadCloser
	Size         int64
	ContentType  string
	ETag         string
	LastModified time.Time
	ContentRange string
	StatusCode   int
}

type Store interface {
	Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error
	Get(ctx context.Context, key, rangeHeader string) (*Object, error)
	Head(ctx context.Context, key string) (*Object, error)
	Delete(ctx context.Context, key string) error
	Health(ctx context.Context) error
}
