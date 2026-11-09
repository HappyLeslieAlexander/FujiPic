package auth

import (
	"strings"
	"testing"

	"fujipic/internal/id"
)

func TestPasswordHashAndVerify(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$") {
		t.Fatalf("hash format = %q", hash)
	}
	ok, err := VerifyPassword("correct horse battery staple", hash)
	if err != nil || !ok {
		t.Fatalf("verify = %v, %v", ok, err)
	}
	ok, err = VerifyPassword("wrong password", hash)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("wrong password verified")
	}
	if _, err := VerifyPassword("x", "not-a-hash"); err == nil {
		t.Fatal("malformed hash accepted")
	}
}

func TestNewToken(t *testing.T) {
	plain, hash, prefix := NewToken()
	if !strings.HasPrefix(plain, TokenPrefix) {
		t.Fatalf("token prefix = %q", plain)
	}
	if len(plain) < 40 {
		t.Fatalf("token too short: %d", len(plain))
	}
	if hash != id.Hash(plain) {
		t.Fatal("hash mismatch")
	}
	if !strings.HasPrefix(plain, prefix) || len(prefix) != 10 {
		t.Fatalf("display prefix = %q", prefix)
	}
	plain2, hash2, _ := NewToken()
	if plain == plain2 || hash == hash2 {
		t.Fatal("tokens are not unique")
	}
}

func TestNormalizeScopes(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "upload,read,delete", true},
		{"upload", "upload", true},
		{"upload,read", "upload,read", true},
		{"read, upload", "read,upload", true},
		{"admin", "", false},
		{"upload,upload", "", false},
		{",", "", false},
	}
	for _, c := range cases {
		got, ok := NormalizeScopes(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("NormalizeScopes(%q) = %q,%v; want %q,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestHasScope(t *testing.T) {
	if !HasScope("upload,read,delete", "read") {
		t.Fatal("read scope not found")
	}
	if HasScope("upload,read", "delete") {
		t.Fatal("delete scope should be absent")
	}
	if HasScope("", "upload") {
		t.Fatal("empty scopes should not match")
	}
}
