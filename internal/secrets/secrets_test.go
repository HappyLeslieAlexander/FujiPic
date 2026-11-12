package secrets

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

func testKey() string {
	return base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
}

func TestRoundTripAndAADBinding(t *testing.T) {
	c, err := NewCipher(testKey())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := c.Encrypt("super-secret", "bkt_1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded, "super-secret") {
		t.Fatal("ciphertext contains plaintext")
	}
	got, err := c.Decrypt(encoded, "bkt_1")
	if err != nil || got != "super-secret" {
		t.Fatalf("Decrypt = %q, %v", got, err)
	}
	if _, err := c.Decrypt(encoded, "bkt_2"); err == nil {
		t.Fatal("ciphertext accepted with a different AAD")
	}
	if _, err := c.Decrypt(encoded[:len(encoded)-2]+"AA", "bkt_1"); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
}

func TestDecodeKeyEncodings(t *testing.T) {
	raw := []byte("0123456789abcdef0123456789abcdef")
	for name, encoded := range map[string]string{
		"base64":     base64.StdEncoding.EncodeToString(raw),
		"raw-base64": base64.RawStdEncoding.EncodeToString(raw),
		"url":        base64.URLEncoding.EncodeToString(raw),
		"hex":        hex.EncodeToString(raw),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := DecodeKey(encoded)
			if err != nil || string(got) != string(raw) {
				t.Fatalf("DecodeKey = %x, %v", got, err)
			}
		})
	}
}

func TestInvalidKeys(t *testing.T) {
	for _, encoded := range []string{"", "not-a-key", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := NewCipher(encoded); encoded != "" && err == nil {
			t.Fatalf("NewCipher(%q) accepted invalid key", encoded)
		}
	}
	if c, err := NewCipher(""); err != nil || c != nil {
		t.Fatalf("empty key = %v, %v; want nil, nil", c, err)
	}
}
