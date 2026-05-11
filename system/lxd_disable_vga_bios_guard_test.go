package system

import (
	"strings"
	"testing"
)

// TestLXDSetConfigRejectsDisableVGAOnBIOS — confirms LXDSetConfig refuses
// disable_virtual_vga=true when Firmware=="bios". Empirical finding from
// 192.168.2.5 (Z370 + UHD 630, Incus 6.0.5, QEMU 10.x, May 2026): the
// guest hangs forever in SeaBIOS during display init because SeaBIOS
// writes boot output only to VGA and Intel iGPUs have no standalone VBIOS
// option ROM. Even the full Q35 IGD recipe (igd-passthru=on, iGPU at
// pcie.0:02.0, x-vga + x-igd-opregion + x-igd-gms) doesn't recover —
// confirmed by 85% CPU spin on one vCPU and empty serial console buffer.
// UEFI (OVMF) handles Intel iGPU init natively via OpRegion, so the
// option is safe on UEFI VMs.
//
// The guard is intentionally a hard error (not silent coercion) so an
// admin attempting it via direct API call sees the explanation rather
// than getting an unworkable VM that hangs in firmware on next start.
func TestLXDSetConfigRejectsDisableVGAOnBIOS(t *testing.T) {
	cfg := LXDInstanceConfig{
		IsVM:              true,
		Firmware:          "bios",
		DisableVirtualVGA: true,
	}
	// Use an instance name pattern that passes lxdNameRe so we reach the
	// guard rather than failing on the name check first.
	err := LXDSetConfig("nonexistent-test-vm", cfg)
	if err == nil {
		t.Fatal("expected error when DisableVirtualVGA=true on BIOS firmware, got nil")
	}
	msg := err.Error()
	// The error must mention both the BIOS context and a clear remedy
	// (switch to UEFI or keep the virtual VGA). Future maintainers might
	// rephrase but those two ideas must remain.
	if !strings.Contains(strings.ToLower(msg), "bios") {
		t.Errorf("error should mention BIOS context: %q", msg)
	}
	if !strings.Contains(strings.ToLower(msg), "uefi") {
		t.Errorf("error should point user at UEFI as the remedy: %q", msg)
	}
}

// TestLXDSetConfigAllowsDisableVGAOnUEFI — sanity check: the guard fires
// ONLY on Firmware=="bios". A UEFI VM with disable_virtual_vga=true must
// not be rejected here; it'll fail later in the flow if the VM doesn't
// exist, but never at the BIOS guard. We just verify the error message
// (if any) does NOT carry the BIOS-guard text — i.e., the guard let it
// through.
func TestLXDSetConfigAllowsDisableVGAOnUEFI(t *testing.T) {
	cfg := LXDInstanceConfig{
		IsVM:              true,
		Firmware:          "uefi",
		DisableVirtualVGA: true,
	}
	err := LXDSetConfig("nonexistent-test-vm", cfg)
	// The call WILL likely fail (no such VM, no incus to talk to in
	// unit-test env), but the error must not be the BIOS guard.
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "bios") &&
		strings.Contains(strings.ToLower(err.Error()), "disable_virtual_vga") {
		t.Errorf("BIOS guard fired on UEFI firmware — should be allowed: %q", err)
	}
}

// TestLXDCreateVMRejectsDisableVGAOnBIOS — same guard, create path.
// Catches the bad combo before `incus init` runs, so we never leave a
// half-created VM behind on a rejected request.
func TestLXDCreateVMRejectsDisableVGAOnBIOS(t *testing.T) {
	req := LXDCreateVMRequest{
		Name:              "nonexistent-test-vm",
		Firmware:          "bios",
		DisableVirtualVGA: true,
	}
	err := LXDCreateVM(req, nil)
	if err == nil {
		t.Fatal("expected error when DisableVirtualVGA=true on BIOS firmware, got nil")
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "bios") || !strings.Contains(msg, "uefi") {
		t.Errorf("error should mention BIOS + UEFI remedy: %q", err)
	}
}
