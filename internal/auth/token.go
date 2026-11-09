package auth

import (
	"strings"

	"fujipic/internal/id"
)

const TokenPrefix = "fp_"

// NewToken returns (plaintext, sha256 hash, display prefix).
func NewToken() (plaintext, hash, prefix string) {
	plaintext = TokenPrefix + id.Random(32)
	hash = id.Hash(plaintext)
	prefix = plaintext
	if len(prefix) > 10 {
		prefix = prefix[:10]
	}
	return plaintext, hash, prefix
}

// NormalizeScopes validates a requested scope string against the supported set.
func NormalizeScopes(raw string) (string, bool) {
	if strings.TrimSpace(raw) == "" {
		return "upload,read,delete", true
	}
	allowed := map[string]bool{"upload": true, "read": true, "delete": true}
	seen := map[string]bool{}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if !allowed[part] || seen[part] {
			return "", false
		}
		seen[part] = true
		out = append(out, part)
	}
	if len(out) == 0 {
		return "", false
	}
	return strings.Join(out, ","), true
}
