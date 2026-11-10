// Package bucket manages the server default S3 bucket and user-owned
// S3-compatible buckets. Credentials for user buckets are encrypted with
// STORAGE_ENCRYPTION_KEY before they are written to SQLite.
package bucket

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"fujipic/internal/id"
	"fujipic/internal/model"
	"fujipic/internal/secrets"
	"fujipic/internal/storage"
	"fujipic/internal/store"
)

var (
	ErrNoEncryptionKey = errors.New("storage encryption key is not configured")
	ErrNotFound        = errors.New("storage bucket not found")
	ErrInUse           = errors.New("storage bucket still has images")
	ErrInvalid         = errors.New("invalid storage bucket")
)

var bucketNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{1,254}$`)

// Factory constructs a storage client from a bucket row and its decrypted
// secret. Production uses S3; tests use in-memory stores.
type Factory func(ctx context.Context, b model.StorageBucket, secret string) (storage.Store, error)

// AddInput is the untrusted form/API input for a new bucket.
type AddInput struct {
	Name            string
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	ForcePathStyle  bool
}

// Registry resolves bucket IDs to storage clients and owns bucket metadata.
type Registry struct {
	store        *store.Store
	defaultStore storage.Store
	cipher       *secrets.Cipher
	factory      Factory
	requireHTTPS bool

	mu    sync.Mutex
	cache map[string]storage.Store
}

// NewRegistry builds a registry. An empty encryption key disables user bucket
// creation but keeps the default bucket fully functional.
func NewRegistry(st *store.Store, defaultStore storage.Store, encodedKey string, factory Factory, requireHTTPS bool) (*Registry, error) {
	if st == nil || defaultStore == nil {
		return nil, errors.New("bucket registry requires a store and a default storage backend")
	}
	cipher, err := secrets.NewCipher(encodedKey)
	if err != nil {
		return nil, fmt.Errorf("STORAGE_ENCRYPTION_KEY: %w", err)
	}
	if cipher != nil && factory == nil {
		return nil, errors.New("bucket factory is required when STORAGE_ENCRYPTION_KEY is set")
	}
	return &Registry{
		store:        st,
		defaultStore: defaultStore,
		cipher:       cipher,
		factory:      factory,
		requireHTTPS: requireHTTPS,
		cache:        make(map[string]storage.Store),
	}, nil
}

// Enabled reports whether operators can add user-owned buckets.
func (r *Registry) Enabled() bool { return r.cipher != nil }

// Default returns the server-wide bucket configured through S3_*.
func (r *Registry) Default() storage.Store { return r.defaultStore }

// List returns metadata for a user's buckets. Secrets are never included.
func (r *Registry) List(ctx context.Context, userID string) ([]model.StorageBucket, error) {
	return r.store.ListStorageBuckets(ctx, userID)
}

// Resolve returns the storage client for an image's bucket ID. An empty ID
// means the server default bucket.
func (r *Registry) Resolve(ctx context.Context, bucketID string) (storage.Store, error) {
	if bucketID == "" {
		return r.defaultStore, nil
	}
	if r.cipher == nil {
		return nil, ErrNoEncryptionKey
	}
	r.mu.Lock()
	cached, ok := r.cache[bucketID]
	r.mu.Unlock()
	if ok {
		return cached, nil
	}
	b, err := r.store.StorageBucketByID(ctx, bucketID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return r.storeFor(ctx, b)
}

// ResolveForUpload validates ownership before returning a bucket client.
func (r *Registry) ResolveForUpload(ctx context.Context, userID, bucketID string) (storage.Store, *model.StorageBucket, error) {
	if bucketID == "" {
		return r.defaultStore, nil, nil
	}
	if r.cipher == nil {
		return nil, nil, ErrNoEncryptionKey
	}
	b, err := r.store.StorageBucketForUser(ctx, userID, bucketID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	objects, err := r.storeFor(ctx, b)
	if err != nil {
		return nil, nil, err
	}
	return objects, b, nil
}

// Add validates a new bucket, proves that it is writable, and stores it.
func (r *Registry) Add(ctx context.Context, userID string, in AddInput) (*model.StorageBucket, error) {
	if r.cipher == nil {
		return nil, ErrNoEncryptionKey
	}
	normalized, err := r.validate(ctx, in)
	if err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	b := &model.StorageBucket{
		ID:             id.New("bkt"),
		UserID:         userID,
		Name:           normalized.Name,
		Endpoint:       normalized.Endpoint,
		Region:         normalized.Region,
		Bucket:         normalized.Bucket,
		AccessKeyID:    normalized.AccessKeyID,
		ForcePathStyle: normalized.ForcePathStyle,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	b.SecretCiphertext, err = r.cipher.Encrypt(normalized.SecretAccessKey, b.ID)
	if err != nil {
		return nil, err
	}
	objects, err := r.factory(ctx, *b, normalized.SecretAccessKey)
	if err != nil {
		return nil, fmt.Errorf("%w: 无法创建存储客户端: %v", ErrInvalid, err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := probe(probeCtx, objects); err != nil {
		return nil, fmt.Errorf("%w: 无法读写存储桶，请检查端点、区域和凭据: %v", ErrInvalid, err)
	}
	if err := r.store.CreateStorageBucket(ctx, b); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return nil, fmt.Errorf("%w: 名称已存在", ErrInvalid)
		}
		return nil, err
	}
	r.mu.Lock()
	r.cache[b.ID] = objects
	r.mu.Unlock()
	return b, nil
}

// Delete removes a bucket registration after all images and retries are gone.
func (r *Registry) Delete(ctx context.Context, userID, bucketID string) error {
	if bucketID == "" {
		return ErrNotFound
	}
	err := r.store.DeleteStorageBucket(ctx, userID, bucketID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, store.ErrBucketInUse):
		return ErrInUse
	case err != nil:
		return err
	}
	r.mu.Lock()
	delete(r.cache, bucketID)
	r.mu.Unlock()
	return nil
}

func (r *Registry) storeFor(ctx context.Context, b *model.StorageBucket) (storage.Store, error) {
	r.mu.Lock()
	if cached, ok := r.cache[b.ID]; ok {
		r.mu.Unlock()
		return cached, nil
	}
	r.mu.Unlock()

	secret, err := r.cipher.Decrypt(b.SecretCiphertext, b.ID)
	if err != nil {
		return nil, fmt.Errorf("decrypt bucket %s credentials: %w", b.ID, err)
	}
	objects, err := r.factory(ctx, *b, secret)
	if err != nil {
		return nil, fmt.Errorf("create storage client for bucket %s: %w", b.ID, err)
	}
	r.mu.Lock()
	if cached, ok := r.cache[b.ID]; ok {
		r.mu.Unlock()
		return cached, nil
	}
	r.cache[b.ID] = objects
	r.mu.Unlock()
	return objects, nil
}

func (r *Registry) validate(ctx context.Context, in AddInput) (AddInput, error) {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" || len([]rune(in.Name)) > 80 {
		return in, fmt.Errorf("%w: 名称长度需在 1 到 80 个字符之间", ErrInvalid)
	}
	in.Endpoint = strings.TrimRight(strings.TrimSpace(in.Endpoint), "/")
	if in.Endpoint != "" {
		u, err := url.Parse(in.Endpoint)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" ||
			(u.Path != "" && u.Path != "/") {
			return in, fmt.Errorf("%w: 端点必须是无路径的 http(s) URL", ErrInvalid)
		}
		if u.Scheme != "https" {
			if r.requireHTTPS || u.Scheme != "http" {
				return in, fmt.Errorf("%w: 生产环境端点必须使用 https", ErrInvalid)
			}
		}
		if err := r.checkPublicHost(ctx, u.Hostname()); err != nil {
			return in, err
		}
	}
	in.Region = strings.TrimSpace(in.Region)
	if in.Region == "" {
		return in, fmt.Errorf("%w: 区域不能为空", ErrInvalid)
	}
	in.Bucket = strings.TrimSpace(in.Bucket)
	if !bucketNamePattern.MatchString(in.Bucket) {
		return in, fmt.Errorf("%w: 桶名称不合法", ErrInvalid)
	}
	in.AccessKeyID = strings.TrimSpace(in.AccessKeyID)
	in.SecretAccessKey = strings.TrimSpace(in.SecretAccessKey)
	if in.AccessKeyID == "" || in.SecretAccessKey == "" {
		return in, fmt.Errorf("%w: Access Key ID 和 Secret Access Key 都不能为空", ErrInvalid)
	}
	return in, nil
}

func (r *Registry) checkPublicHost(ctx context.Context, host string) error {
	if host == "" {
		return fmt.Errorf("%w: 端点缺少主机名", ErrInvalid)
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(lookupCtx, host)
	if err != nil {
		return fmt.Errorf("%w: 无法解析端点主机: %v", ErrInvalid, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("%w: 端点主机没有地址", ErrInvalid)
	}
	for _, ip := range ips {
		if !isPublicIP(ip.IP) {
			return fmt.Errorf("%w: 端点不能指向内网、回环或保留地址", ErrInvalid)
		}
	}
	return nil
}

func probe(ctx context.Context, objects storage.Store) error {
	key := ".fujipic-probe/" + id.Random(12)
	const body = "fujipic"
	if err := objects.Put(ctx, key, strings.NewReader(body), int64(len(body)), "text/plain"); err != nil {
		return err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = objects.Delete(cleanupCtx, key)
	}()
	obj, err := objects.Get(ctx, key, "")
	if err != nil {
		return err
	}
	return obj.Body.Close()
}

// PublicHTTPClient returns an HTTP client that only dials public IPs. It is
// used for user-supplied endpoints so a bucket cannot be pointed at internal
// services (SSRF), including after DNS rebinding.
func PublicHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			var lastErr error
			for _, ip := range ips {
				if !isPublicIP(ip.IP) {
					continue
				}
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.IP.String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, fmt.Errorf("endpoint %s has no public address", host)
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConns:          10,
		ForceAttemptHTTP2:     true,
	}
	return &http.Client{Transport: transport, Timeout: 5 * time.Minute}
}

func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 0: // 0.0.0.0/8
			return false
		case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127: // 100.64.0.0/10 CGNAT
			return false
		case v4[0] == 192 && v4[1] == 0 && v4[2] == 0: // 192.0.0.0/24
			return false
		case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19): // 198.18.0.0/15
			return false
		case v4[0] >= 240: // 240.0.0.0/4
			return false
		}
	}
	return true
}
