package server

import "testing"

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
