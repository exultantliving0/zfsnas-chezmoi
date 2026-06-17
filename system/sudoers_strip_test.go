package system

import (
	"strings"
	"testing"
)

// TestStrippedSudoersHasNoVirtualizationWording verifies that on a host without
// virtualization enabled (the default test process — version.IsExperimental()
// is false, so RequiredSudoersContent returns the stripped form), the sudoers
// file contains NO virtualization commands, aliases, or explanatory wording.
// Regression guard for the v6.6.16 cleanup (install-only ZFSNAS_VMSETUP removed;
// virt runtime aliases — including zram memory compression — plus their comment
// blocks stripped when the feature is off).
func TestStrippedSudoersHasNoVirtualizationWording(t *testing.T) {
	off := RequiredSudoersContent()

	// Genuinely non-virtualization features must remain.
	for _, keep := range []string{"ZFSNAS_FILES", "ZFSNAS_JOURNAL", "ZFSNAS_SMART"} {
		if !strings.Contains(off, keep) {
			t.Errorf("stripped sudoers missing expected non-virt content %q", keep)
		}
	}

	// No virtualization aliases, commands, or wording — anywhere (rules OR
	// comments). Memory compression (zram / ZFSNAS_MEMCOMP) is part of the
	// virtualization feature, so it must NOT appear on a non-virt host either.
	forbidden := []string{
		"ZFSNAS_INCUS", "ZFSNAS_INCUSNET", "ZFSNAS_SYNCOID", "ZFSNAS_VMSETUP",
		"ZFSNAS_MEMCOMP", "zramswap", "zram", "swapoff", "Memory Compression",
		"incus", "Incus", "Proxmox", "OVMF", "virtualiz", "Virtualiz",
		"VMs & Containers", "syncoid", "VM/Container", "Container Console",
	}
	for _, f := range forbidden {
		if strings.Contains(off, f) {
			for _, ln := range strings.Split(off, "\n") {
				if strings.Contains(ln, f) {
					t.Errorf("stripped sudoers leaks virtualization token %q in line: %q", f, ln)
					break
				}
			}
		}
	}
}
