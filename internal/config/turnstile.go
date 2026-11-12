package config

import (
	"errors"
	"net/netip"
	"strings"
	"unicode"
)

// TurnstileEnabled is true for any supplied setting so partial configuration
// cannot silently disable verification. Validate requires all three settings.
func (c *Config) TurnstileEnabled() bool {
	return c.TurnstileSiteKey != "" || c.TurnstileSecret != "" || len(c.TurnstileHostnames) != 0
}

func (c *Config) validateTurnstile() error {
	if !c.TurnstileEnabled() {
		return nil
	}
	if c.TurnstileSiteKey == "" || c.TurnstileSecret == "" || len(c.TurnstileHostnames) == 0 {
		return errors.New("TURNSTILE_SITE_KEY, TURNSTILE_SECRET and TURNSTILE_HOSTNAMES must all be configured")
	}
	if len(c.TurnstileSiteKey) > 256 || strings.ContainsFunc(c.TurnstileSiteKey, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-')
	}) || len(c.TurnstileSecret) > 1024 || strings.ContainsFunc(c.TurnstileSecret, unicode.IsSpace) {
		return errors.New("invalid TURNSTILE key format")
	}
	if c.IsProduction() {
		for _, prefix := range []string{"1x00000000000000000000", "2x00000000000000000000", "3x00000000000000000000"} {
			if strings.HasPrefix(c.TurnstileSiteKey, prefix) || strings.HasPrefix(c.TurnstileSecret, prefix) {
				return errors.New("TURNSTILE test keys are not allowed in production")
			}
		}
	}
	for i, host := range c.TurnstileHostnames {
		host = strings.ToLower(strings.TrimSpace(host))
		if host == "" || len(host) > 253 {
			return errors.New("TURNSTILE_HOSTNAMES requires exact hostnames without URLs, ports or wildcards")
		}
		if addr, err := netip.ParseAddr(host); err == nil {
			if c.IsProduction() && (addr.IsLoopback() || addr.IsUnspecified()) {
				return errors.New("TURNSTILE_HOSTNAMES must not include loopback hosts in production")
			}
		} else {
			for _, label := range strings.Split(host, ".") {
				if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' || strings.ContainsFunc(label, func(r rune) bool {
					return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-')
				}) {
					return errors.New("TURNSTILE_HOSTNAMES requires exact hostnames without URLs, ports or wildcards")
				}
			}
			if c.IsProduction() && (host == "localhost" || strings.HasSuffix(host, ".localhost")) {
				return errors.New("TURNSTILE_HOSTNAMES must not include localhost in production")
			}
		}
		c.TurnstileHostnames[i] = host
	}
	return nil
}
