package config

import (
	"strings"
	"testing"
)

func TestTurnstileConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, site, secret, host string
		production, valid        bool
	}{
		{"disabled", "", "", "", true, true},
		{"enabled", "site-key", "secret-key", "pic.example.com", true, true},
		{"normalized", "site-key", "secret-key", " PIC.EXAMPLE.COM ", true, true},
		{"site-only", "site-key", "", "", true, false},
		{"secret-only", "", "secret-key", "", true, false},
		{"host-only", "", "", "pic.example.com", true, false},
		{"missing-host", "site-key", "secret-key", "", true, false},
		{"url", "site-key", "secret-key", "https://pic.example.com", true, false},
		{"wildcard", "site-key", "secret-key", "*.example.com", true, false},
		{"port", "site-key", "secret-key", "pic.example.com:443", true, false},
		{"localhost", "site-key", "secret-key", "localhost", true, false},
		{"loopback", "site-key", "secret-key", "127.0.0.1", true, false},
		{"ipv6-loopback", "site-key", "secret-key", "::1", true, false},
		{"dev", "site-key", "secret-key", "localhost", false, true},
		{"test-key", "1x00000000000000000000AA", "secret-key", "pic.example.com", true, false},
		{"test-secret", "site-key", "1x0000000000000000000000000000000AA", "pic.example.com", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			if tc.production {
				c.StorageDriver = "s3"
				c.S3Bucket, c.S3AccessKeyID, c.S3SecretAccessKey = "test", "test", "test"
				c.AppEnv = "production"
				c.AppBaseURL = "https://pic.example.com"
			}
			c.TurnstileSiteKey, c.TurnstileSecret = tc.site, tc.secret
			if tc.host != "" {
				c.TurnstileHostnames = []string{tc.host}
			}
			err := c.Validate()
			if (err == nil) != tc.valid {
				t.Fatalf("Validate()=%v valid=%v", err, tc.valid)
			}
			if err != nil && strings.Contains(err.Error(), "secret-key") {
				t.Fatal("configuration error exposed secret")
			}
		})
	}
}
