// Package config loads and validates all runtime configuration from the
// environment. Secrets are never logged or returned through the API.
package config

import (
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
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

	PublicBaseURL string

	MaxUploadBytes    int64
	MaxImagePixels    int64
	MaxImageDimension int

	DefaultQuotaBytes     int64
	AllowOpenRegistration bool

	BootstrapAdminEmail    string
	BootstrapAdminPassword string

	PublicContact string // abuse-report email shown in page footers; empty hides it

	TrustProxy     bool
	ClientIPHeader string
	OriginSecret   string

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
		S3ForcePathStyle:       envBool("S3_FORCE_PATH_STYLE", false),
		PublicBaseURL:          strings.TrimRight(env("PUBLIC_BASE_URL", ""), "/"),
		MaxUploadBytes:         envInt64("MAX_UPLOAD_BYTES", 10<<20),
		MaxImagePixels:         envInt64("MAX_IMAGE_PIXELS", 50_000_000),
		MaxImageDimension:      int(envInt64("MAX_IMAGE_DIMENSION", 12000)),
		DefaultQuotaBytes:      envInt64("DEFAULT_QUOTA_BYTES", 1<<30),
		AllowOpenRegistration:  envBool("ALLOW_OPEN_REGISTRATION", false),
		BootstrapAdminEmail:    env("BOOTSTRAP_ADMIN_EMAIL", ""),
		BootstrapAdminPassword: env("BOOTSTRAP_ADMIN_PASSWORD", ""),
		PublicContact:          env("CONTACT_EMAIL", ""),
		TrustProxy:             envBool("TRUST_PROXY", false),
		ClientIPHeader:         env("CLIENT_IP_HEADER", "CF-Connecting-IP"),
		OriginSecret:           env("ORIGIN_SHARED_SECRET", ""),
		SessionTTL:             envDuration("SESSION_TTL", 30*24*time.Hour),
		TokenTTL:               envDuration("TOKEN_TTL", 0),
		CDNPurgeProvider:       strings.ToLower(env("CDN_PURGE_PROVIDER", "none")),
		CDNPurgeEndpoint:       env("CDN_PURGE_ENDPOINT", ""),
		CDNPurgeToken:          env("CDN_PURGE_TOKEN", ""),
		CDNZoneID:              env("CDN_ZONE_ID", ""),
		LogLevel:               env("LOG_LEVEL", "info"),
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
	}
	if c.PublicBaseURL != "" {
		if err := validHTTPURL(c.PublicBaseURL); err != nil {
			errs = append(errs, fmt.Errorf("PUBLIC_BASE_URL: %w", err))
		} else if u, _ := url.Parse(c.PublicBaseURL); u.Path != "" && u.Path != "/" {
			errs = append(errs, errors.New("PUBLIC_BASE_URL must not contain a path"))
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
	return errors.Join(errs...)
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
	if u.Host == "" {
		return fmt.Errorf("missing host in %q", raw)
	}
	return nil
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return b
}

func envInt64(key string, def int64) int64 {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return def
	}
	return n
}

func envDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return d
}
