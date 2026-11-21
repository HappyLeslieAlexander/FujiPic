// Package id generates opaque identifiers and hashes tokens.
package id

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// New returns a prefixed random identifier, e.g. img_7f3k9q2m....
// The random part is 16 bytes (128 bits) encoded as 26 base32 characters.
func New(prefix string) string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return prefix + "_" + b32.EncodeToString(buf)
}

// Random returns n random bytes encoded with URL-safe base64 without padding.
func Random(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(buf)
}

// RandomBase32 returns n random bytes encoded as lowercase base32 without
// padding. 16 bytes produce 26 characters, suitable for object keys.
func RandomBase32(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return strings.ToLower(b32.EncodeToString(buf))
}

// Hash returns the hex SHA-256 of a token. Only hashes are stored in SQLite.
func Hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
