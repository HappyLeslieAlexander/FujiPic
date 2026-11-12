package config

import "testing"

func TestImageAliasValidation(t *testing.T) {
	for _, raw := range []string{
		"https://img.example.com", "https://alias.example.com/path", "https://alias.example.com?",
		"https://alias.example.com?q=1", "https://alias.example.com#x", "https://user:pass@alias.example.com",
		"http://alias.example.com", "https://pic.example.com", "javascript:alert(1)",
		"https://pic.example.com:443", "https://PIC.example.com.",
		"https://img.example.com:443", "https://img.example.com.", "https://alias.example.com:65536",
	} {
		t.Run(raw, func(t *testing.T) {
			c := validConfig()
			c.AppEnv = "production"
			c.AppBaseURL = "https://pic.example.com"
			c.PublicBaseAliases = []string{raw}
			if c.Validate() == nil {
				t.Fatal("invalid alias accepted")
			}
		})
	}
	c := validConfig()
	c.PublicBaseAliases = []string{" https://ALT.example.com/ "}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.PublicBaseAliases[0] != "https://alt.example.com" {
		t.Fatal("alias not normalized")
	}
	c.PublicBaseAliases = append(c.PublicBaseAliases, "https://ALT.example.com/")
	if c.Validate() == nil {
		t.Fatal("duplicate alias accepted")
	}
}

func TestImageAliasesLoad(t *testing.T) {
	t.Setenv("APP_ENV", "development")
	t.Setenv("STORAGE_DRIVER", "memory")
	t.Setenv("PUBLIC_BASE_URL", "https://img.example.com")
	t.Setenv("PUBLIC_BASE_ALIASES", "https://alt.example.com/, https://other.example.com")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.PublicBaseAliases) != 2 || c.PublicBaseAliases[0] != "https://alt.example.com" {
		t.Fatal("aliases not loaded")
	}
}
