// Package secrets encrypts small application secrets (currently per-user
// S3 credentials) with AES-256-GCM. The encryption key is supplied by the
// operator through STORAGE_ENCRYPTION_KEY and never stored in SQLite.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// DecodeKey accepts a 32-byte key encoded as base64 (standard or URL-safe,
// padded or unpadded) or as 64 hexadecimal characters.
func DecodeKey(encoded string) ([]byte, error) {
	encoded = strings.TrimSpace(encoded)
	if encoded == "" {
		return nil, errors.New("key is empty")
	}
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if key, err := enc.DecodeString(encoded); err == nil && len(key) == 32 {
			return key, nil
		}
	}
	if key, err := hex.DecodeString(encoded); err == nil && len(key) == 32 {
		return key, nil
	}
	return nil, errors.New("key must be 32 bytes encoded as base64 or hex")
}

// Cipher is an AES-256-GCM encryptor. A nil *Cipher is valid and reports that
// encryption is not configured.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher builds a Cipher from an encoded key. An empty key returns (nil, nil)
// so single-bucket deployments can run without the multi-bucket feature.
func NewCipher(encodedKey string) (*Cipher, error) {
	if strings.TrimSpace(encodedKey) == "" {
		return nil, nil
	}
	key, err := DecodeKey(encodedKey)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt returns base64(nonce || ciphertext || tag). aad binds the ciphertext
// to a stable identifier such as the bucket row ID.
func (c *Cipher) Encrypt(plaintext, aad string) (string, error) {
	if c == nil {
		return "", errors.New("encryption key is not configured")
	}
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	out := c.aead.Seal(nonce, nonce, []byte(plaintext), []byte(aad))
	return base64.RawStdEncoding.EncodeToString(out), nil
}

// Decrypt reverses Encrypt and fails if the ciphertext or AAD was changed.
func (c *Cipher) Decrypt(encoded, aad string) (string, error) {
	if c == nil {
		return "", errors.New("encryption key is not configured")
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return "", fmt.Errorf("decode ciphertext: %w", err)
	}
	if len(raw) < c.aead.NonceSize() {
		return "", errors.New("ciphertext is too short")
	}
	nonce, ciphertext := raw[:c.aead.NonceSize()], raw[c.aead.NonceSize():]
	plain, err := c.aead.Open(nil, nonce, ciphertext, []byte(aad))
	if err != nil {
		return "", errors.New("decrypt failed: wrong key or corrupted data")
	}
	return string(plain), nil
}
