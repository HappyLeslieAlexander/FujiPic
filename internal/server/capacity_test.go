package server

import (
	"strings"
	"testing"
)

func TestCapacityRejectsExcessivePrecisionAndUnderflowExponents(t *testing.T) {
	for _, raw := range []string{"1e-100000", "0e-100000", "0." + strings.Repeat("0", 1000) + "1"} {
		if _, err := parseCapacityGiB(raw); err == nil {
			t.Errorf("unbounded numeric input accepted: %.40s", raw)
		}
	}
}

func TestParseCapacityGiB(t *testing.T) {
	cases := map[string]int64{
		"":      0,
		"0":     0,
		"1":     int64(1) << 30,
		"0.5":   int64(1) << 29,
		"0.001": 1073741,
	}
	for raw, want := range cases {
		got, err := parseCapacityGiB(raw)
		if err != nil || got != want {
			t.Errorf("parseCapacityGiB(%q) = %d, %v; want %d", raw, got, err, want)
		}
	}
	// 0 means unlimited, so a tiny positive value must not round down to it.
	if got, err := parseCapacityGiB("0.0000000001"); err != nil || got != 1 {
		t.Fatalf("tiny capacity = %d, %v; want 1 byte", got, err)
	}
	for _, raw := range []string{"-1", "NaN", "Inf", "1e10", "abc"} {
		if _, err := parseCapacityGiB(raw); err == nil {
			t.Errorf("parseCapacityGiB(%q) accepted invalid capacity", raw)
		}
	}
}

// TestCapacityGiBRoundTrip guards the admin bucket edit form: the value shown
// in the max_gb input must parse back to the exact stored byte count. In
// particular, tiny positive capacities must not render as "0.000", which the
// form would then save as unlimited.
func TestCapacityGiBRoundTrip(t *testing.T) {
	const maxBytes = int64(1_000_000_000) << 30
	for _, want := range []int64{
		0, 1, 512, 1 << 20, 1073741, 1 << 30, (1 << 30) + 1, 123456789,
		// Values above float64's exact integer range used to drift by
		// tens of bytes when the admin form round-tripped them.
		1<<53 + 1, 577366096992549791, 864588681177041180,
		302136984794790379, 1<<59 + 12345, maxBytes - 1, maxBytes,
	} {
		raw := capacityGiB(want)
		got, err := parseCapacityGiB(raw)
		if err != nil || got != want {
			t.Errorf("capacityGiB(%d) = %q, parse = %d, %v; want %d", want, raw, got, err, want)
		}
	}
	if got := capacityGiB(1); got == "0" || got == "0.000" {
		t.Fatalf("tiny capacity formatted as %q, would save as unlimited", got)
	}
	if _, err := parseCapacityGiB("1000000001"); err == nil {
		t.Fatal("capacity above the 1e9 GiB limit was accepted")
	}
}

// TestQuotaMiBRoundTrip guards the admin user quota form. DEFAULT_QUOTA_BYTES
// may be any positive byte count, so formatting it as integer MiB used to
// shrink the quota silently when an admin saved the form unchanged.
func TestQuotaMiBRoundTrip(t *testing.T) {
	const maxBytes = int64(1<<20) << 20
	for _, want := range []int64{
		1 << 20, (1 << 20) + 1, 1500000, 1 << 30, 123456789,
		1<<40 - 1, maxBytes,
	} {
		raw := quotaMiB(want)
		got, err := parseQuotaMiB(raw)
		if err != nil || got != want {
			t.Errorf("quotaMiB(%d) = %q, parse = %d, %v; want %d", want, raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "0", "0.999", "1048577", "-1", "NaN", "Inf", "1e999999999"} {
		if _, err := parseQuotaMiB(raw); err == nil {
			t.Errorf("parseQuotaMiB(%q) accepted invalid quota", raw)
		}
	}
}

func TestQuotaPercentDoesNotOverflow(t *testing.T) {
	const maxInt64 = int64(^uint64(0) >> 1)
	if got := quotaPercent(maxInt64, maxInt64); got != 100 {
		t.Fatalf("quotaPercent(max, max) = %d, want 100", got)
	}
	if got := quotaPercent(maxInt64/2, maxInt64); got != 49 {
		t.Fatalf("quotaPercent(max/2, max) = %d, want 49", got)
	}
	if got := quotaPercent(maxInt64/2+1, maxInt64); got != 50 {
		t.Fatalf("quotaPercent(max/2+1, max) = %d, want 50", got)
	}
	if got := quotaPercent(1, maxInt64); got != 0 {
		t.Fatalf("quotaPercent(1, max) = %d, want 0", got)
	}
	if got := quotaPercent(5, 0); got != 0 {
		t.Fatalf("quotaPercent(5, 0) = %d, want 0", got)
	}
	if got := quotaPercent(6, 5); got != 100 {
		t.Fatalf("quotaPercent(6, 5) = %d, want 100", got)
	}
}
