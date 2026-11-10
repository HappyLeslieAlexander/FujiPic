// Package bucket manages the global pool of S3-compatible storage buckets.
// Administrators add buckets, assign capacity and priority, and uploads are
// routed automatically to the highest-priority bucket with free capacity.
// Credentials are encrypted with STORAGE_ENCRYPTION_KEY before they reach
// SQLite.
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
	ErrNoCapacity      = store.ErrBucketCapacity
	ErrDefaultBucket   = store.ErrDefaultBucket
)

const (
	// DefaultName is the display name of the bucket configured by S3_*.
	DefaultName = "服务器默认存储"
	// DefaultPriority places the environment bucket after newly added buckets
	// unless an administrator changes it.
	DefaultPriority = 1000
)

var bucketNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{1,254}$`)

// Factory constructs a storage client from a bucket row and its decrypted
// secret. Production uses S3; tests use in-memory stores.
type Factory func(ctx context.Context, b model.StorageBucket, secret string) (storage.Store, error)

// AddInput is the untrusted admin form input for a new bucket.
type AddInput struct {
	Name            string
	Endpoint        string
	Region          string
	Bucket          string
	AccessKeyID     string
	SecretAccessKey string
	ForcePathStyle  bool
	MaxBytes        int64
	Priority        int
}

// UpdateInput changes pool settings without exposing credentials.
type UpdateInput struct {
	Name     string
	MaxBytes int64
	Priority int
	Enabled  bool
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

// NewRegistry builds a registry. An empty encryption key disables adding
// custom buckets but keeps the environment bucket fully functional.
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

// Enabled reports whether an administrator can add custom buckets.
func (r *Registry) Enabled() bool { return r.cipher != nil }

// Default returns the server-wide bucket configured through S3_*.
func (r *Registry) Default() storage.Store { return r.defaultStore }

// EnsureDefault creates the fixed default row and reconciles capacity
// counters. It must run before uploads are accepted.
func (r *Registry) EnsureDefault(ctx context.Context, endpoint, region, bucket string, forcePathStyle bool) error {
	if err := r.store.EnsureDefaultStorageBucket(ctx, &model.StorageBucket{
		ID:             model.DefaultBucketID,
		Name:           DefaultName,
		Endpoint:       endpoint,
		Region:         region,
		Bucket:         bucket,
		ForcePathStyle: forcePathStyle,
		Priority:       DefaultPriority,
		Enabled:        true,
		IsDefault:      true,
	}); err != nil {
		return err
	}
	return r.store.ReconcileStorageBucketUsage(ctx)
}

// List returns the global pool in selection order. Secrets are never included.
func (r *Registry) List(ctx context.Context) ([]model.StorageBucket, error) {
	return r.store.ListStorageBuckets(ctx)
}

// Resolve returns the storage client for an image's bucket ID. Empty and
// legacy IDs resolve to the environment bucket.
func (r *Registry) Resolve(ctx context.Context, bucketID string) (storage.Store, error) {
	if bucketID == "" || bucketID == model.DefaultBucketID {
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

// SelectForUpload atomically reserves size bytes in the first enabled bucket
// with free capacity. The caller must call Release if the upload fails.
func (r *Registry) SelectForUpload(ctx context.Context, size int64) (storage.Store, *model.StorageBucket, error) {
	b, err := r.store.ReserveStorageBucket(ctx, size)
	if err != nil {
		if errors.Is(err, store.ErrBucketCapacity) {
			return nil, nil, ErrNoCapacity
		}
		return nil, nil, err
	}
	if b.IsDefault || b.ID == model.DefaultBucketID {
		return r.defaultStore, b, nil
	}
	objects, err := r.storeFor(ctx, b)
	if err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		_ = r.store.ReleaseStorageBucket(cleanupCtx, b.ID, size)
		cancel()
		return nil, nil, err
	}
	return objects, b, nil
}

// Release returns reserved capacity after a failed upload.
func (r *Registry) Release(ctx context.Context, bucketID string, size int64) error {
	return r.store.ReleaseStorageBucket(ctx, bucketID, size)
}

// Add validates a new bucket, proves that it is writable, and stores it.
func (r *Registry) Add(ctx context.Context, in AddInput) (*model.StorageBucket, error) {
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
		Name:           normalized.Name,
		Endpoint:       normalized.Endpoint,
		Region:         normalized.Region,
		Bucket:         normalized.Bucket,
		AccessKeyID:    normalized.AccessKeyID,
		ForcePathStyle: normalized.ForcePathStyle,
		MaxBytes:       normalized.MaxBytes,
		Priority:       normalized.Priority,
		Enabled:        true,
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

// Update changes capacity, priority, enabled state, and (for custom buckets)
// the display name.
func (r *Registry) Update(ctx context.Context, bucketID string, in UpdateInput) error {
	if bucketID == "" {
		return ErrNotFound
	}
	b, err := r.store.StorageBucketByID(ctx, bucketID)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if in.MaxBytes < 0 {
		return fmt.Errorf("%w: 最大容量不能为负数", ErrInvalid)
	}
	if in.MaxBytes > 0 && in.MaxBytes < b.UsedBytes {
		return fmt.Errorf("%w: 最大容量不能小于当前已用容量", ErrInvalid)
	}
	if in.Priority < 0 || in.Priority > 1_000_000 {
		return fmt.Errorf("%w: 优先级需在 0 到 1000000 之间", ErrInvalid)
	}
	name := b.Name
	if !b.IsDefault {
		name = strings.TrimSpace(in.Name)
		if name == "" || len([]rune(name)) > 80 {
			return fmt.Errorf("%w: 名称长度需在 1 到 80 个字符之间", ErrInvalid)
		}
		if name == DefaultName {
			return fmt.Errorf("%w: 该名称保留给服务器默认存储桶", ErrInvalid)
		}
	}
	b.Name = name
	b.MaxBytes = in.MaxBytes
	b.Priority = in.Priority
	b.Enabled = in.Enabled
	if err := r.store.UpdateStorageBucket(ctx, b); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return fmt.Errorf("%w: 名称已存在", ErrInvalid)
		}
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	return nil
}

// Delete removes a non-default bucket after all images and retries are gone.
func (r *Registry) Delete(ctx context.Context, bucketID string) error {
	if bucketID == "" {
		return ErrNotFound
	}
	err := r.store.DeleteStorageBucket(ctx, bucketID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, store.ErrBucketInUse):
		return ErrInUse
	case errors.Is(err, store.ErrDefaultBucket):
		return ErrDefaultBucket
	case err != nil:
		return err
	}
	r.mu.Lock()
	delete(r.cache, bucketID)
	r.mu.Unlock()
	return nil
}

func (r *Registry) storeFor(ctx context.Context, b *model.StorageBucket) (storage.Store, error) {
	if b.IsDefault || b.ID == model.DefaultBucketID {
		return r.defaultStore, nil
	}
	if r.cipher == nil {
		return nil, ErrNoEncryptionKey
	}
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
	if in.Name == DefaultName {
		return in, fmt.Errorf("%w: 该名称保留给服务器默认存储桶", ErrInvalid)
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
	if in.MaxBytes < 0 {
		return in, fmt.Errorf("%w: 最大容量不能为负数", ErrInvalid)
	}
	if in.Priority < 0 || in.Priority > 1_000_000 {
		return in, fmt.Errorf("%w: 优先级需在 0 到 1000000 之间", ErrInvalid)
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
// used for administrator-supplied endpoints so a bucket cannot be pointed at
// internal services (SSRF), including after DNS rebinding.
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
