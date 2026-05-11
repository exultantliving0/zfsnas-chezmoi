package system

import (
	"strings"
	"testing"
)

// TestApplyPCIRawQEMUStripsOrphanSetForRemovedDevice — regression for the
// 6.5.23 → 6.5.24 bug on mediaserver. After v6.5.23 made applyPCIRawQEMU
// preserve admin-added properties so they survive Edit-then-Save, the
// strip step ALSO preserved them when the device itself was removed.
// Removing the iGPU passthrough in the UI left
// `-set device.dev-incus_pci0.x-igd-opregion=on` in raw.qemu pointing at a
// device that no longer existed — QEMU then refused to start the VM with:
//
//	-set device.dev-incus_pci0.x-igd-opregion=on:
//	there is no device "dev-incus_pci0" defined
//
// Fix in v6.5.24: orphan-strip rule. If the device name is absent from
// the current pciDevices list, every `-set device.dev-incus_<gone>.*`
// line is stripped regardless of property — even admin-added ones —
// because there's nothing for them to refer to anymore.
//
// In v6.5.26 x-igd-opregion was promoted to UI-managed, so the test now
// uses x-no-mmap as the representative admin-only property to actually
// exercise the orphan-strip code path (pciManagedSetRe would strip
// managed props on its own without needing the orphan rule).
func TestApplyPCIRawQEMUStripsOrphanSetForRemovedDevice(t *testing.T) {
	got := simulateApplyPCIRawQEMU(
		"-set device.dev-incus_pci0.x-no-mmap=on -smbios type=1,uuid=abc",
		nil, // pciDevices = no devices left
	)
	if strings.Contains(got, "dev-incus_pci0") {
		t.Errorf("orphan -set for removed dev-incus_pci0 not stripped: %q", got)
	}
	if !strings.Contains(got, "-smbios type=1,uuid=abc") {
		t.Errorf("unrelated raw.qemu content must survive: %q", got)
	}
}

// TestApplyPCIRawQEMUPreservesAdminPropOnExistingDevice — keep the
// existing 6.5.23 invariant: when the device IS still in pciDevices,
// admin-added properties pass through. Pin this alongside the new
// orphan-strip rule so a future "simplify" refactor can't accidentally
// lump the two cases together.
//
// Uses x-no-mmap (admin-only since the v6.5.26 IGD promotion) as the
// representative non-managed property. Originally used x-igd-opregion,
// which became UI-managed in v6.5.26.
func TestApplyPCIRawQEMUPreservesAdminPropOnExistingDevice(t *testing.T) {
	got := simulateApplyPCIRawQEMU(
		"-set device.dev-incus_pci0.x-no-mmap=on -smbios type=1",
		[]LXDPCIDevice{{DeviceName: "pci0", Address: "0000:00:02.0"}},
	)
	if !strings.Contains(got, "x-no-mmap=on") {
		t.Errorf("admin-added prop lost while device still exists: %q", got)
	}
}

// TestApplyPCIRawQEMUStripsOrphanButKeepsRetainedDevice — multiple PCI
// devices, one removed, one retained. Only the orphan side gets stripped;
// the retained device keeps its admin props.
func TestApplyPCIRawQEMUStripsOrphanButKeepsRetainedDevice(t *testing.T) {
	input := strings.Join([]string{
		"-set device.dev-incus_pci0.x-igd-opregion=on",
		"-set device.dev-incus_pci1.x-no-mmap=on",
		"-smbios type=1,uuid=abc",
	}, " ")

	// Keep pci1, drop pci0.
	got := simulateApplyPCIRawQEMU(input, []LXDPCIDevice{{DeviceName: "pci1", Address: "0000:00:1f.6"}})

	if strings.Contains(got, "dev-incus_pci0") {
		t.Errorf("removed device's -set not stripped: %q", got)
	}
	if !strings.Contains(got, "dev-incus_pci1.x-no-mmap=on") {
		t.Errorf("retained device's admin prop lost: %q", got)
	}
	if !strings.Contains(got, "-smbios") {
		t.Errorf("surrounding content lost: %q", got)
	}
}

// simulateApplyPCIRawQEMU re-runs the regex-only portion of
// applyPCIRawQEMU (the part that doesn't shell out to `incus config`) so
// the strip behavior can be tested in isolation. Mirrors lxd.go ~1056-1090.
func simulateApplyPCIRawQEMU(existing string, pciDevices []LXDPCIDevice) string {
	existing = strings.TrimSpace(existing)
	// We're not testing the legacy `-device vfio-pci,...` shape here.
	existing = pciManagedSetRe.ReplaceAllString(existing, "")

	keep := map[string]bool{}
	for _, pci := range pciDevices {
		if pci.DeviceName != "" {
			keep["dev-incus_"+pci.DeviceName] = true
		}
	}
	existing = pciOrphanSetRe.ReplaceAllStringFunc(existing, func(m string) string {
		sub := pciOrphanSetRe.FindStringSubmatch(m)
		if len(sub) < 2 {
			return m
		}
		if keep[sub[1]] {
			return m
		}
		return ""
	})
	existing = strings.TrimSpace(existing)

	var newEntries []string
	for _, pci := range pciDevices {
		if arg := buildPCIQEMUArg(pci); arg != "" {
			newEntries = append(newEntries, arg)
		}
	}
	parts := []string{}
	if existing != "" {
		parts = append(parts, existing)
	}
	parts = append(parts, newEntries...)
	return strings.Join(parts, " ")
}
