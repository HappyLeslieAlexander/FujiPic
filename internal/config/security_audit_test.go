package config

import (
	"strings"
	"testing"
)

func TestProductionPublicOriginsAndPurgeValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Config)
	}{
		{"APP_BASE_URL", func(c *Config) { c.AppBaseURL = "http://app.test" }},
		{"PUBLIC_BASE_URL", func(c *Config) { c.PublicBaseURL = "http://img.test" }},
		{"ORIGIN_SHARED_SECRET", func(c *Config) { c.OriginSecret = "" }},
		{"CDN_PURGE_ENDPOINT", func(c *Config) { c.CDNPurgeEndpoint = "http://purge.test" }},
		{"CDN_PURGE_ENDPOINT", func(c *Config) { c.CDNPurgeEndpoint = "https://user:password@purge.test" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			c.AppEnv, c.AppBaseURL = "production", "https://app.test"
			c.StorageDriver, c.S3Bucket, c.S3AccessKeyID, c.S3SecretAccessKey = "s3", "test", "test", "test"
			c.CDNPurgeProvider, c.CDNPurgeEndpoint, c.CDNPurgeToken = "generic", "https://purge.test", "test"
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
			tc.change(c)
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tc.name) {
				t.Fatal(err)
			}
		})
	}
}
