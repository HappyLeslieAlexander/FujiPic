package config

import (
	"strings"
	"testing"
)

func validConfig() *Config {
	return &Config{
		AppEnv:            "development",
		AppBaseURL:        "http://localhost:8080",
		DatabasePath:      "./data/fujipic.db",
		TmpDir:            "/tmp",
		StorageDriver:     "memory",
		PublicBaseURL:     "https://img.example.com",
		MaxUploadBytes:    10 << 20,
		MaxImagePixels:    50_000_000,
		MaxImageDimension: 12000,
		DefaultQuotaBytes: 1 << 30,
		SessionTTL:        24 * 60 * 60 * 1e9,
		CDNPurgeProvider:  "none",
	}
}

func TestValidateValid(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"bad env", func(c *Config) { c.AppEnv = "staging" }, "APP_ENV"},
		{"bad app url", func(c *Config) { c.AppBaseURL = "ftp://x" }, "APP_BASE_URL"},
		{"missing public", func(c *Config) { c.PublicBaseURL = "" }, "PUBLIC_BASE_URL is required"},
		{"public with path", func(c *Config) { c.PublicBaseURL = "https://img.example.com/prefix" }, "must not contain a path"},
		{"zero upload", func(c *Config) { c.MaxUploadBytes = 0 }, "MAX_UPLOAD_BYTES"},
		{"bad storage", func(c *Config) { c.StorageDriver = "ftp" }, "STORAGE_DRIVER"},
		{"s3 missing creds", func(c *Config) { c.StorageDriver = "s3" }, "S3_BUCKET"},
		{"cloudflare missing zone", func(c *Config) { c.CDNPurgeProvider = "cloudflare" }, "CDN_ZONE_ID"},
		{"generic missing endpoint", func(c *Config) { c.CDNPurgeProvider = "generic" }, "CDN_PURGE_ENDPOINT"},
		{"bootstrap without password", func(c *Config) { c.BootstrapAdminEmail = "a@b.com" }, "BOOTSTRAP_ADMIN_PASSWORD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(c)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestContactEmail(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	t.Setenv("APP_BASE_URL", "http://localhost:8080")
	t.Setenv("PUBLIC_BASE_URL", "https://img.example.com")
	t.Setenv("STORAGE_DRIVER", "memory")

	t.Setenv("CONTACT_EMAIL", "abuse@example.com")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PublicContact != "abuse@example.com" {
		t.Fatalf("PublicContact = %q", cfg.PublicContact)
	}

	t.Setenv("CONTACT_EMAIL", "not-an-email")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "CONTACT_EMAIL") {
		t.Fatalf("invalid contact error = %v", err)
	}

	t.Setenv("CONTACT_EMAIL", "")
	if _, err := Load(); err != nil {
		t.Fatalf("empty contact must be allowed: %v", err)
	}
}

func TestLoadFromEnv(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	t.Setenv("APP_BASE_URL", "http://localhost:9999")
	t.Setenv("PUBLIC_BASE_URL", "https://img.example.com")
	t.Setenv("STORAGE_DRIVER", "memory")
	t.Setenv("MAX_UPLOAD_BYTES", "2048")
	t.Setenv("ALLOW_OPEN_REGISTRATION", "true")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxUploadBytes != 2048 || !c.AllowOpenRegistration {
		t.Fatalf("loaded config = %+v", c)
	}
	if c.SecureCookies() {
		t.Fatal("development HTTP should not force secure cookies")
	}
}

func TestSecureCookies(t *testing.T) {
	c := validConfig()
	c.AppEnv = "production"
	if !c.SecureCookies() {
		t.Fatal("production should use secure cookies")
	}
	c.AppEnv = "development"
	c.AppBaseURL = "https://pic.example.com"
	if !c.SecureCookies() {
		t.Fatal("https base URL should use secure cookies")
	}
}
