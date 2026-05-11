package system

import (
	"strings"
	"testing"
)

// TestBuildPCIQEMUArgEmitsIGDOpRegion — x-igd-opregion=On in the UI must
// emit `-set device.dev-incus_<name>.x-igd-opregion=on`. Boolean, so the
// "1" the dropdown stores becomes the QEMU token "on" via pciBoolToken.
//
// Required for stable Intel iGPU passthrough: without OpRegion, the i915
// driver in the guest wedges after a few seconds of sustained VAAPI load.
// Verified empirically on mediaserver (Z370 + UHD 630, May 2026).
func TestBuildPCIQEMUArgEmitsIGDOpRegion(t *testing.T) {
	got := buildPCIQEMUArg(LXDPCIDevice{
		DeviceName:   "pci0",
		Address:      "0000:00:02.0",
		XIGDOpRegion: "1",
	})
	want := "-set device.dev-incus_pci0.x-igd-opregion=on"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// TestBuildPCIQEMUArgEmitsIGDGMS — x-igd-gms is numeric (uint32, 0..16);
// passes through as-is without bool normalisation. "2" stays "2" (=64MB).
func TestBuildPCIQEMUArgEmitsIGDGMS(t *testing.T) {
	got := buildPCIQEMUArg(LXDPCIDevice{
		DeviceName: "pci0",
		Address:    "0000:00:02.0",
		XIGDGMS:    "2",
	})
	want := "-set device.dev-incus_pci0.x-igd-gms=2"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// TestBuildPCIQEMUArgFullIGDRecipe — the canonical Intel iGPU render-only
// passthrough config: OpRegion on, GMS=2 (64MB), every other knob default.
// Pins the order of fields too — if a future refactor reorders the emitter,
// the test catches it (raw.qemu is whitespace-sensitive for the QEMU parser).
func TestBuildPCIQEMUArgFullIGDRecipe(t *testing.T) {
	got := buildPCIQEMUArg(LXDPCIDevice{
		DeviceName:   "pci0",
		Address:      "0000:00:02.0",
		XIGDOpRegion: "1",
		XIGDGMS:      "2",
	})
	want := "-set device.dev-incus_pci0.x-igd-opregion=on -set device.dev-incus_pci0.x-igd-gms=2"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// TestBuildPCIQEMUArgIGDDisabledEmitsNothing — empty string for both IGD
// fields and the rest of the device defaults means "no overrides at all";
// buildPCIQEMUArg returns "" so applyPCIRawQEMU can skip emission and not
// leave a trailing space in raw.qemu.
func TestBuildPCIQEMUArgIGDDisabledEmitsNothing(t *testing.T) {
	got := buildPCIQEMUArg(LXDPCIDevice{
		DeviceName: "pci0",
		Address:    "0000:00:02.0",
	})
	if got != "" {
		t.Errorf("expected empty arg for fully-default device, got %q", got)
	}
}

// TestPciManagedSetReCoversIGDProps — the strip regex must match
// x-igd-opregion and x-igd-gms so re-saving (with the same UI values)
// removes the previous emission instead of accumulating duplicates.
// Mirrors the existing coverage for rombar/x-vga/aer.
func TestPciManagedSetReCoversIGDProps(t *testing.T) {
	for _, p := range []string{"x-igd-opregion=on", "x-igd-gms=2"} {
		input := "-set device.dev-incus_pci0." + p
		got := pciManagedSetRe.ReplaceAllString(input, "")
		if strings.TrimSpace(got) != "" {
			t.Errorf("strip regex failed to remove managed IGD prop %q: leftover=%q", p, got)
		}
	}
}

// TestPciManagedSetReStillPreservesNonManaged — once we widened the
// allowlist to include the two IGD props, make sure we didn't accidentally
// catch other admin-added properties on the same device. x-no-mmap=on
// (sometimes used as a workaround on broken IOMMU groups) must still pass
// through unchanged.
func TestPciManagedSetReStillPreservesNonManaged(t *testing.T) {
	input := "-set device.dev-incus_pci0.x-no-mmap=on"
	got := pciManagedSetRe.ReplaceAllString(input, "")
	if strings.TrimSpace(got) == "" {
		t.Errorf("strip regex over-reached and removed admin x-no-mmap prop: %q", input)
	}
}
