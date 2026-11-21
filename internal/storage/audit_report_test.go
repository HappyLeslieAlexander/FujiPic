package storage

import (
	"context"
	"testing"
	"time"
)

func TestDefaultS3ClientHasOverallTimeout(t *testing.T) {
	s, err := NewS3(context.Background(), S3Options{Region: "us-east-1", Bucket: "test", AccessKeyID: "test", SecretAccessKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	c, ok := s.client.Options().HTTPClient.(interface{ GetTimeout() time.Duration })
	if !ok || c.GetTimeout() != 5*time.Minute {
		t.Fatal("default S3 timeout missing")
	}
}
