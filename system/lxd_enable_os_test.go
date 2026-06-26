package system

import "testing"

// TestDebianVersionAtLeast covers the v6.6.26 OS-floor logic: a real Debian
// must be 13+ to qualify, while testing/sid (no VERSION_ID) is a rolling
// release newer than any stable and must be allowed.
func TestDebianVersionAtLeast(t *testing.T) {
	cases := []struct {
		versionID string
		minMajor  int
		want      bool
	}{
		{"13", 13, true},     // Debian 13 (trixie) — the floor
		{"14", 13, true},     // newer stable
		{"12", 13, false},    // Debian 12 (bookworm) — too old
		{"11", 13, false},    // Debian 11 (bullseye) — too old
		{"", 13, true},       // testing/sid carries no VERSION_ID → rolling, allow
		{"  ", 13, true},     // whitespace-only → treated as empty
		{"trixie", 13, true}, // codename instead of number → assume newer/rolling
	}
	for _, c := range cases {
		if got := debianVersionAtLeast(c.versionID, c.minMajor); got != c.want {
			t.Errorf("debianVersionAtLeast(%q, %d) = %v, want %v", c.versionID, c.minMajor, got, c.want)
		}
	}
}

// TestMinVirtRAMThreshold documents the "more than 4 GB" Hardware Requirements
// rule: a 4 GiB host (MemTotal ≈ 4.0–4.1×10⁹ after kernel reservation) passes,
// while a host with 4 GB (decimal) or less fails.
func TestMinVirtRAMThreshold(t *testing.T) {
	cases := []struct {
		ram  uint64
		want bool // > minVirtRAMBytes
	}{
		{4 * 1024 * 1024 * 1024, true},  // 4 GiB ≈ 4.29e9 → pass
		{4*1000*1000*1000 + 1, true},    // just over 4 GB → pass
		{4 * 1000 * 1000 * 1000, false}, // exactly 4 GB → fail (must be MORE than)
		{2 * 1024 * 1024 * 1024, false}, // 2 GiB → fail
		{0, false},                      // unknown/zero → fail
	}
	for _, c := range cases {
		if got := c.ram > minVirtRAMBytes; got != c.want {
			t.Errorf("ram=%d > minVirtRAMBytes(%d) = %v, want %v", c.ram, minVirtRAMBytes, got, c.want)
		}
	}
}
