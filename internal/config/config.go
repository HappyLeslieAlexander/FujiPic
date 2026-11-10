// Package config loads and validates all runtime configuration from the
// environment. Secrets are never logged or returned through the API.
package config

import (
	"errors"
	"fmt"
	"net/mail"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"fujipic/internal/secrets"
)

type Config struct {
	AppEnv       string
	AppBaseURL   string
	ListenAddr   string
	DatabasePath string
	TmpDir       string

	StorageDriver string

	S3Endpoint        string
	S3Region          string
	S3Bucket          string
	S3AccessKeyID     string
	S3SecretAccessKey string
	S3ForcePathStyle  bool

	// StorageEncryptionKey protects admin-added bucket credentials in SQLite.
	// Empty disables custom buckets but keeps the S3_* default bucket working.
	StorageEncryptionKey string

	PublicBaseURL string

	MaxUploadBytes    int64
	MaxImagePixels    int64
	MaxImageDimension int

	DefaultQuotaBytes     int64
	AllowOpenRegistration bool

	BootstrapAdminEmail    string
	BootstrapAdminPassword string

	PublicContact string // abuse-report email shown in page footers; empty hides it

	TrustProxy        bool
	TrustedProxyCIDRs []string
	ClientIPHeader    string
	OriginSecret      string

	SessionTTL time.Duration
	TokenTTL   time.Duration

	CDNPurgeProvider string
	CDNPurgeEndpoint string
	CDNPurgeToken    string
	CDNZoneID        string

	LogLevel string
}

// Load reads configuration from the process environment.
func Load() (*Config, error) {
	var parseErrs []error
	boolValue := func(key string, def bool) bool {
		value, err := envBool(key, def)
		if err != nil {
			parseErrs = append(parseErrs, err)
		}
		return value
	}
	intValue := func(key string, def int64) int64 {
		value, err := envInt64(key, def)
		if err != nil {
			parseErrs = append(parseErrs, err)
		}
		return value
	}
	durationValue := func(key string, def time.Duration) time.Duration {
		value, err := envDuration(key, def)
		if err != nil {
			parseErrs = append(parseErrs, err)
		}
		return value
	}
	c := &Config{
		AppEnv:                 env("APP_ENV", "production"),
		AppBaseURL:             strings.TrimRight(env("APP_BASE_URL", "http://localhost:8080"), "/"),
		ListenAddr:             env("LISTEN_ADDR", ":8080"),
		DatabasePath:           env("DATABASE_PATH", "./data/fujipic.db"),
		TmpDir:                 env("TMP_DIR", os.TempDir()),
		StorageDriver:          env("STORAGE_DRIVER", "s3"),
		S3Endpoint:             env("S3_ENDPOINT", ""),
		S3Region:               env("S3_REGION", "auto"),
		S3Bucket:               env("S3_BUCKET", ""),
		S3AccessKeyID:          env("S3_ACCESS_KEY_ID", ""),
		S3SecretAccessKey:      env("S3_SECRET_ACCESS_KEY", ""),
		S3ForcePathStyle:       boolValue("S3_FORCE_PATH_STYLE", false),
		StorageEncryptionKey:   env("STORAGE_ENCRYPTION_KEY", ""),
		PublicBaseURL:          strings.TrimRight(env("PUBLIC_BASE_URL", ""), "/"),
		MaxUploadBytes:         intValue("MAX_UPLOAD_BYTES", 10<<20),
		MaxImagePixels:         intValue("MAX_IMAGE_PIXELS", 50_000_000),
		MaxImageDimension:      int(intValue("MAX_IMAGE_DIMENSION", 12000)),
		DefaultQuotaBytes:      intValue("DEFAULT_QUOTA_BYTES", 1<<30),
		AllowOpenRegistration:  boolValue("ALLOW_OPEN_REGISTRATION", false),
		BootstrapAdminEmail:    env("BOOTSTRAP_ADMIN_EMAIL", ""),
		BootstrapAdminPassword: env("BOOTSTRAP_ADMIN_PASSWORD", ""),
		PublicContact:          env("CONTACT_EMAIL", ""),
		TrustProxy:             boolValue("TRUST_PROXY", false),
		TrustedProxyCIDRs:      envStringList("TRUSTED_PROXY_CIDRS"),
		ClientIPHeader:         env("CLIENT_IP_HEADER", "CF-Connecting-IP"),
		OriginSecret:           env("ORIGIN_SHARED_SECRET", ""),
		SessionTTL:             durationValue("SESSION_TTL", 30*24*time.Hour),
		TokenTTL:               durationValue("TOKEN_TTL", 0),
		CDNPurgeProvider:       strings.ToLower(env("CDN_PURGE_PROVIDER", "none")),
		CDNPurgeEndpoint:       env("CDN_PURGE_ENDPOINT", ""),
		CDNPurgeToken:          env("CDN_PURGE_TOKEN", ""),
		CDNZoneID:              env("CDN_ZONE_ID", ""),
		LogLevel:               env("LOG_LEVEL", "info"),
	}

	if len(parseErrs) > 0 {
		return nil, errors.Join(parseErrs...)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) Validate() error {
	var errs []error
	if c.AppEnv != "production" && c.AppEnv != "development" {
		errs = append(errs, fmt.Errorf("APP_ENV must be production or development, got %q", c.AppEnv))
	}
	if err := validHTTPURL(c.AppBaseURL); err != nil {
		errs = append(errs, fmt.Errorf("APP_BASE_URL: %w", err))
	} else if u, _ := url.Parse(c.AppBaseURL); (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		errs = append(errs, errors.New("APP_BASE_URL must not contain a path, query or fragment"))
	}
	if c.PublicBaseURL != "" {
		if err := validHTTPURL(c.PublicBaseURL); err != nil {
			errs = append(errs, fmt.Errorf("PUBLIC_BASE_URL: %w", err))
		} else if u, _ := url.Parse(c.PublicBaseURL); (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			errs = append(errs, errors.New("PUBLIC_BASE_URL must not contain a path, query or fragment"))
		}
	} else {
		errs = append(errs, errors.New("PUBLIC_BASE_URL is required (CDN image domain)"))
	}
	if c.MaxUploadBytes <= 0 {
		errs = append(errs, errors.New("MAX_UPLOAD_BYTES must be positive"))
	}
	if c.MaxImagePixels <= 0 || c.MaxImageDimension <= 0 {
		errs = append(errs, errors.New("MAX_IMAGE_PIXELS and MAX_IMAGE_DIMENSION must be positive"))
	}
	if c.DefaultQuotaBytes <= 0 {
		errs = append(errs, errors.New("DEFAULT_QUOTA_BYTES must be positive"))
	}
	if c.SessionTTL <= 0 {
		errs = append(errs, errors.New("SESSION_TTL must be positive"))
	}
	switch c.StorageDriver {
	case "s3":
		if c.S3Bucket == "" || c.S3AccessKeyID == "" || c.S3SecretAccessKey == "" {
			errs = append(errs, errors.New("S3_BUCKET, S3_ACCESS_KEY_ID and S3_SECRET_ACCESS_KEY are required when STORAGE_DRIVER=s3"))
		}
	case "memory":
		// Used by tests and local demos; no credentials needed.
	default:
		errs = append(errs, fmt.Errorf("STORAGE_DRIVER must be s3 or memory, got %q", c.StorageDriver))
	}
	if c.StorageEncryptionKey != "" {
		if _, err := secrets.DecodeKey(c.StorageEncryptionKey); err != nil {
			errs = append(errs, fmt.Errorf("STORAGE_ENCRYPTION_KEY: %w", err))
		}
	}
	switch c.CDNPurgeProvider {
	case "none", "":
	case "cloudflare":
		if c.CDNZoneID == "" || c.CDNPurgeToken == "" {
			errs = append(errs, errors.New("CDN_PURGE_PROVIDER=cloudflare requires CDN_ZONE_ID and CDN_PURGE_TOKEN"))
		}
	case "generic":
		if c.CDNPurgeEndpoint == "" || c.CDNPurgeToken == "" {
			errs = append(errs, errors.New("CDN_PURGE_PROVIDER=generic requires CDN_PURGE_ENDPOINT and CDN_PURGE_TOKEN"))
		}
	default:
		errs = append(errs, fmt.Errorf("CDN_PURGE_PROVIDER must be none, cloudflare or generic, got %q", c.CDNPurgeProvider))
	}
	if c.BootstrapAdminEmail != "" && c.BootstrapAdminPassword == "" {
		errs = append(errs, errors.New("BOOTSTRAP_ADMIN_PASSWORD is required when BOOTSTRAP_ADMIN_EMAIL is set"))
	}
	if c.PublicContact != "" {
		addr, err := mail.ParseAddress(c.PublicContact)
		if err != nil || addr.Address != c.PublicContact {
			errs = append(errs, fmt.Errorf("CONTACT_EMAIL must be a single mailbox address, got %q", c.PublicContact))
		}
	}
	if c.TrustProxy && len(c.TrustedProxyCIDRs) == 0 {
		errs = append(errs, errors.New("TRUSTED_PROXY_CIDRS is required when TRUST_PROXY=true"))
	}
	for _, cidr := range c.TrustedProxyCIDRs {
		if _, err := parseTrustedPrefix(cidr); err != nil {
			errs = append(errs, fmt.Errorf("TRUSTED_PROXY_CIDRS invalid %q: %w", cidr, err))
		}
	}
	return errors.Join(errs...)
}

func parseTrustedPrefix(raw string) (netip.Prefix, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return netip.Prefix{}, fmt.Errorf("empty CIDR")
	}
	if strings.Contains(raw, "/") {
		return netip.ParsePrefix(raw)
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(addr, addr.BitLen()), nil
}

// IsTrustedProxy reports whether the direct peer is an allow-listed proxy.
func (c *Config) IsTrustedProxy(remoteIP netip.Addr) bool {
	if !remoteIP.IsValid() {
		return false
	}
	for _, raw := range c.TrustedProxyCIDRs {
		if prefix, err := parseTrustedPrefix(raw); err == nil && prefix.Contains(remoteIP) {
			return true
		}
	}
	return false
}

func envStringList(key string) []string {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func (c *Config) SecureCookies() bool {
	return c.AppEnv == "production" || strings.HasPrefix(c.AppBaseURL, "https://")
}

func (c *Config) IsProduction() bool { return c.AppEnv == "production" }

func validHTTPURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("must start with http:// or https://, got %q", raw)
	}
	if u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("missing host in %q", raw)
	}
	if u.User != nil {
		return fmt.Errorf("must not contain credentials in %q", raw)
	}
	return nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envBool(key string, def bool) (bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return def, fmt.Errorf("%s: invalid boolean: %w", key, err)
	}
	return b, nil
}

func envInt64(key string, def int64) (int64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return def, fmt.Errorf("%s: invalid integer: %w", key, err)
	}
	return n, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def, nil
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return def, fmt.Errorf("%s: invalid duration: %w", key, err)
	}
	return d, nil
}
