package system

import (
	"strings"
	"testing"
)

// TestPciManagedSetRePreservesUnknownProps — the regex used to strip ZNAS'
// own per-device vfio-pci overrides MUST only match properties the UI
// manages (rombar / x-vga / aer / x-igd-opregion / x-igd-gms as of
// v6.5.26). Anything else on device.dev-incus_<name> was added by the
// admin out-of-band — most commonly x-no-mmap=on or vendor-id overrides
// for broken IOMMU groups — and stripping it would silently break the
// admin's workaround on every unrelated Edit save.
//
// History: this test was originally written in v6.5.23 with x-igd-opregion
// as the admin-preserved prop, after a v6.5.22 regression destroyed it.
// In v6.5.26 we promoted x-igd-opregion + x-igd-gms to UI-managed fields
// (Intel iGPU recipe shipped as a first-class UI feature), so the test
// switched to x-no-mmap as the representative admin-only property.
func TestPciManagedSetRePreservesUnknownProps(t *testing.T) {
	input := "-smbios type=1 " +
		"-set device.dev-incus_pci0.x-vga=on " +
		"-set device.dev-incus_pci0.x-igd-opregion=on " +
		"-set device.dev-incus_pci0.rombar=1 " +
		"-set device.dev-incus_pci0.x-no-mmap=on " +
		"-set device.dev-incus_pci0.aer=on " +
		"-set device.dev-incus_pci0.x-igd-gms=2"

	got := pciManagedSetRe.ReplaceAllString(input, "")
	got = strings.TrimSpace(got)

	// Managed properties stripped (UI-emitted; re-added from UI state):
	for _, m := range []string{"x-vga=on", "rombar=1", "aer=on", "x-igd-opregion=on", "x-igd-gms=2"} {
		if strings.Contains(got, m) {
			t.Errorf("managed property %q should be stripped: %q", m, got)
		}
	}

	// Unknown properties preserved verbatim (admin-only, never UI-emitted):
	for _, k := range []string{"x-no-mmap=on"} {
		if !strings.Contains(got, k) {
			t.Errorf("admin-added property %q lost on strip: %q", k, got)
		}
	}

	// Non-PCI flag preserved:
	if !strings.Contains(got, "-smbios type=1") {
		t.Errorf("surrounding flags must survive: %q", got)
	}
}

// TestPciManagedSetReMatchesEachManagedProp — sanity that each of the
// three managed property names is actually matched in isolation. Guards
// against typos in the regex character class.
func TestPciManagedSetReMatchesEachManagedProp(t *testing.T) {
	for _, p := range []string{"rombar=1", "x-vga=on", "aer=on"} {
		input := "-set device.dev-incus_pci0." + p
		out := pciManagedSetRe.ReplaceAllString(input, "")
		if strings.TrimSpace(out) != "" {
			t.Errorf("regex failed to strip managed prop %q: result=%q", p, out)
		}
	}
}
