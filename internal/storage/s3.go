package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

type S3Options struct {
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	ForcePathStyle  bool
	HTTPClient      *http.Client
}

type S3 struct {
	client *s3.Client
	bucket string
}

func NewS3(ctx context.Context, opts S3Options) (*S3, error) {
	loadOpts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(opts.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			opts.AccessKeyID, opts.SecretAccessKey, "")),
	}
	if opts.HTTPClient != nil {
		loadOpts = append(loadOpts, awsconfig.WithHTTPClient(opts.HTTPClient))
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if opts.Endpoint != "" {
			o.BaseEndpoint = aws.String(opts.Endpoint)
		}
		o.UsePathStyle = opts.ForcePathStyle
	})
	return &S3{client: client, bucket: opts.Bucket}, nil
}

func (s *S3) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          body,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(contentType),
		CacheControl:  aws.String("public, max-age=31536000, immutable"),
	})
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", key, err)
	}
	return nil
}

func (s *S3) Get(ctx context.Context, key, rangeHeader string) (*Object, error) {
	input := &s3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)}
	if rangeHeader != "" {
		input.Range = aws.String(rangeHeader)
	}
	out, err := s.client.GetObject(ctx, input)
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		if isInvalidRange(err) {
			// Preserve the Range response contract expected by handlers_image:
			// a zero-length 416 object, not a generic storage failure.
			return &Object{
				Body:        io.NopCloser(strings.NewReader("")),
				ContentType: "text/plain; charset=utf-8",
				StatusCode:  http.StatusRequestedRangeNotSatisfiable,
			}, nil
		}
		return nil, fmt.Errorf("s3 get %s: %w", key, err)
	}
	obj := &Object{
		Body:         out.Body,
		ContentType:  aws.ToString(out.ContentType),
		ETag:         aws.ToString(out.ETag),
		ContentRange: aws.ToString(out.ContentRange),
		StatusCode:   200,
	}
	if out.ContentLength != nil {
		obj.Size = *out.ContentLength
	}
	if out.LastModified != nil {
		obj.LastModified = *out.LastModified
	}
	if obj.ContentRange != "" {
		obj.StatusCode = 206
	}
	return obj, nil
}

func (s *S3) Head(ctx context.Context, key string) (*Object, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("s3 head %s: %w", key, err)
	}
	obj := &Object{
		Body:        io.NopCloser(emptyReader{}),
		ContentType: aws.ToString(out.ContentType),
		ETag:        aws.ToString(out.ETag),
		StatusCode:  200,
	}
	if out.ContentLength != nil {
		obj.Size = *out.ContentLength
	}
	if out.LastModified != nil {
		obj.LastModified = *out.LastModified
	}
	return obj, nil
}

func (s *S3) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("s3 delete %s: %w", key, err)
	}
	return nil
}

func (s *S3) Health(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(s.bucket)})
	return err
}

func isInvalidRange(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode() == "InvalidRange"
	}
	return false
}

func isNotFound(err error) bool {
	var noKey *types.NoSuchKey
	if errors.As(err, &noKey) {
		return true
	}
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }
