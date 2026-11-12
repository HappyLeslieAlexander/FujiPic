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

func TestVerifyPasswordRejectsMalformedHashes(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(hash, "$")
	if len(parts) != 6 {
		t.Fatalf("unexpected hash format: %q", hash)
	}
	// An empty digest would otherwise make ConstantTimeCompare(empty, empty)
	// succeed for every password.
	mutations := map[string]string{
		"empty digest":    strings.Join(append(parts[:5:5], ""), "$"),
		"empty salt":      strings.Join([]string{parts[0], parts[1], parts[2], parts[3], "", parts[5]}, "$"),
		"wrong version":   strings.Replace(hash, "v=19", "v=18", 1),
		"zero memory":     strings.Replace(hash, "m=65536", "m=0", 1),
		"zero iterations": strings.Replace(hash, "t=3", "t=0", 1),
		"zero parallel":   strings.Replace(hash, "p=2", "p=0", 1),
	}
	for name, malformed := range mutations {
		if malformed == hash {
			t.Fatalf("%s mutation did not change the hash", name)
		}
		ok, err := VerifyPassword("correct horse battery staple", malformed)
		if err == nil || ok {
			t.Errorf("%s: VerifyPassword = %v, %v; want ErrInvalidHash", name, ok, err)
		}
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

func TestValidPasswordLengthCountsCharacters(t *testing.T) {
	cases := []struct {
		password string
		want     bool
	}{
		{"123456789", false},
		{"1234567890", true},
		{"密码密码密码", false},    // 6 characters, 18 bytes
		{"密码密码密码密码密码", true}, // 10 characters, 30 bytes
		{strings.Repeat("a", 200), true},
		{strings.Repeat("a", 201), false},
		{strings.Repeat("密", 200), true},
		{strings.Repeat("密", 201), false},
	}
	for _, c := range cases {
		if got := ValidPasswordLength(c.password); got != c.want {
			t.Errorf("ValidPasswordLength(%q) = %v, want %v", c.password, got, c.want)
		}
	}
}
