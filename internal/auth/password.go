// Package auth implements password hashing and API token primitives.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters. 64 MiB / 3 iterations is a reasonable 2026 default
// for a single-instance service with modest login volume.
const (
	argonMemory      = 64 * 1024
	argonIterations  = 3
	argonParallelism = 2
	argonKeyLen      = 32
	argonSaltLen     = 16
)

var ErrInvalidHash = errors.New("invalid password hash")

// Password length limits are expressed in characters, matching the web UI
// and browser minlength/maxlength validation. Measuring bytes would let a
// short non-ASCII password pass the advertised ten-character minimum.
const (
	MinPasswordChars = 10
	MaxPasswordChars = 200
)

// ValidPasswordLength reports whether password contains between
// MinPasswordChars and MaxPasswordChars Unicode code points.
func ValidPasswordLength(password string) bool {
	n := utf8.RuneCountInString(password)
	return n >= MinPasswordChars && n <= MaxPasswordChars
}

// HashPassword returns a PHC-formatted Argon2id hash.
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, argonIterations, argonMemory, argonParallelism, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonIterations, argonParallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// VerifyPassword checks password against a PHC-formatted Argon2id hash.
func VerifyPassword(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrInvalidHash
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return false, ErrInvalidHash
	}
	var memory uint32
	var iterations uint32
	var parallelism uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &iterations, &parallelism); err != nil {
		return false, ErrInvalidHash
	}
	if version != argon2.Version {
		return false, ErrInvalidHash
	}
	// Reject degenerate or absurd parameters. Without these checks a
	// corrupted hash with an empty digest would verify any password, and a
	// huge memory/iteration value would turn login into a DoS primitive.
	if memory < 8 || memory > 256*1024 || iterations < 1 || iterations > 6 ||
		parallelism < 1 || parallelism > 16 {
		return false, ErrInvalidHash
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, ErrInvalidHash
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, ErrInvalidHash
	}
	if len(salt) < 8 || len(salt) > 64 || len(want) < 16 || len(want) > 64 {
		return false, ErrInvalidHash
	}
	got := argon2.IDKey([]byte(password), salt, iterations, memory, parallelism, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyHash is used to keep login timing similar when the email is unknown.
var dummyHash, _ = HashPassword("fujipic-dummy-password-not-used")

// DummyVerify burns approximately the same CPU as a real verification.
func DummyVerify(password string) {
	_, _ = VerifyPassword(password, dummyHash)
}

// HasScope reports whether a comma-separated scope list contains scope.
func HasScope(scopes, scope string) bool {
	for _, s := range strings.Split(scopes, ",") {
		if strings.TrimSpace(s) == scope {
			return true
		}
	}
	return false
}
