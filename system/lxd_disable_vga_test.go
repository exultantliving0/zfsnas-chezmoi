package system

import (
	"strings"
	"testing"
)

// TestApplyDisableVirtualVGAFromEmpty — fresh VM, no prior raw.qemu.conf.
// Enabling must produce exactly the bare section+driver pair (no comments,
// no markers — those broke QEMU's config-file parser in 6.5.21).
func TestApplyDisableVirtualVGAFromEmpty(t *testing.T) {
	got := applyDisableVirtualVGA("", true)
	want := disableVGAOverrideBody
	if got != want {
		t.Errorf("from-empty apply\ngot  = %q\nwant = %q", got, want)
	}
	// Specifically: no `#` markers must ever appear in the rendered body —
	// QEMU's parser hard-fails on a comment line at section position with
	// "Expected section header, got ...".
	if strings.Contains(got, "#") {
		t.Errorf("emitted body must not contain comment lines: %q", got)
	}
}

// TestApplyDisableVirtualVGAUnsetFromEmpty — toggling OFF on a VM that
// never had the override returns "" so the caller can `incus config unset
// raw.qemu.conf` cleanly.
func TestApplyDisableVirtualVGAUnsetFromEmpty(t *testing.T) {
	if got := applyDisableVirtualVGA("", false); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

// TestApplyDisableVirtualVGAStripPreservesOther — the strip regex must
// remove only the ZNAS section+driver pair we wrote, leaving any other
// raw.qemu.conf content (the user's own QEMU overrides) byte-for-byte
// intact.
func TestApplyDisableVirtualVGAStripPreservesOther(t *testing.T) {
	user := `[chardev "user_serial"]
backend = "file"
path = "/var/log/myvm-serial.log"`
	withOverride := user + "\n\n" + disableVGAOverrideBody + "\n"

	got := applyDisableVirtualVGA(withOverride, false)
	if !strings.Contains(got, "user_serial") {
		t.Errorf("user content lost on toggle-off:\n%s", got)
	}
	if strings.Contains(got, "pcie-pci-bridge") {
		t.Errorf("ZNAS override not stripped:\n%s", got)
	}
}

// TestApplyDisableVirtualVGAIdempotent — repeat apply-on must not produce
// duplicate override blocks (re-saving an Edit modal without changing the
// checkbox shouldn't accumulate junk in raw.qemu.conf).
func TestApplyDisableVirtualVGAIdempotent(t *testing.T) {
	once := applyDisableVirtualVGA("", true)
	twice := applyDisableVirtualVGA(once, true)
	if strings.Count(twice, "pcie-pci-bridge") != 1 {
		t.Errorf("idempotency lost: %d pcie-pci-bridge entries in:\n%s",
			strings.Count(twice, "pcie-pci-bridge"), twice)
	}
}

// TestReadDisableVirtualVGAFromUserKey — the GET path reads the
// ZNAS-managed state flag from the instance's user.znas:disable_virtual_vga
// config key. raw.qemu.conf is NOT consulted (it could have been edited by
// the admin out-of-band).
func TestReadDisableVirtualVGAFromUserKey(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]string
		want bool
	}{
		{"empty config", map[string]string{}, false},
		{"key set to true", map[string]string{disableVGAUserKey: "true"}, true},
		{"key set to false", map[string]string{disableVGAUserKey: "false"}, false},
		{"key set to other value", map[string]string{disableVGAUserKey: "yes"}, false},
		{
			"raw.qemu.conf has the override but user key absent → false",
			map[string]string{"raw.qemu.conf": disableVGAOverrideBody},
			false,
		},
	}
	for _, tc := range cases {
		if got := readDisableVirtualVGA(tc.cfg); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
