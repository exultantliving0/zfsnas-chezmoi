package system

import (
	"strings"
	"testing"
)

// TestUpdateRawQEMUMachineStripAtStart — the previous strings.Index lookup
// required a leading space, so a `-machine q35` clause sitting at the very
// start of raw.qemu was never stripped before re-append. Repeat saves then
// accumulated `-machine q35 -machine q35 …`, which made the LAST -machine
// flag (still q35) override any new value the user picked, producing the
// "Machine Type dropdown always reverts to Q35" symptom. Regex strip must
// match a clause at position 0.
func TestUpdateRawQEMUMachineStripAtStart(t *testing.T) {
	got := updateRawQEMUMachine("-machine q35 -smbios type=1,uuid=abc", "pc")
	want := "-smbios type=1,uuid=abc -machine pc"
	if got != want {
		t.Errorf("strip-at-start failed\ngot  = %q\nwant = %q", got, want)
	}
}

// TestUpdateRawQEMUMachineDedupesDuplicates — guards against accumulated
// duplicates from past buggy versions: opening Edit on a VM that already
// shows `-machine q35 -machine q35` must produce a single -machine clause
// after save (not three, four, …).
func TestUpdateRawQEMUMachineDedupesDuplicates(t *testing.T) {
	got := updateRawQEMUMachine("-machine q35 -machine q35 -smbios type=1,uuid=abc", "pc")
	if n := strings.Count(got, "-machine "); n != 1 {
		t.Errorf("expected exactly one -machine clause, got %d in %q", n, got)
	}
	if !strings.Contains(got, "-machine pc") {
		t.Errorf("new machine type not present: %q", got)
	}
	if strings.Contains(got, "-machine q35") {
		t.Errorf("old machine type not stripped: %q", got)
	}
}

// TestUpdateRawQEMUMachineCleanEmpty — passing "" must strip every -machine
// clause (used on the success path of applyConf("qemu.machine.type") so the
// raw.qemu fallback can never disagree with the native key on subsequent
// saves).
func TestUpdateRawQEMUMachineCleanEmpty(t *testing.T) {
	got := updateRawQEMUMachine("-machine q35 -machine q35 -smbios type=1", "")
	want := "-smbios type=1"
	if got != want {
		t.Errorf("clean-empty failed\ngot  = %q\nwant = %q", got, want)
	}
}

// TestUpdateRawQEMUMachineMidString — a -machine clause sitting in the
// middle of raw.qemu (with surrounding flags on both sides) must still be
// stripped cleanly and replaced.
func TestUpdateRawQEMUMachineMidString(t *testing.T) {
	got := updateRawQEMUMachine("-smbios type=1 -machine q35 -set device.foo.bar=on", "pc")
	if strings.Contains(got, "-machine q35") {
		t.Errorf("old clause not stripped: %q", got)
	}
	if !strings.Contains(got, "-machine pc") {
		t.Errorf("new clause not added: %q", got)
	}
	if !strings.Contains(got, "-smbios type=1") || !strings.Contains(got, "-set device.foo.bar=on") {
		t.Errorf("surrounding flags lost: %q", got)
	}
}

// TestUpdateRawQEMUMachineEmptyInput — fresh VM with empty raw.qemu and a
// machine type to set must produce just the new clause, no leading space.
func TestUpdateRawQEMUMachineEmptyInput(t *testing.T) {
	if got := updateRawQEMUMachine("", "pc"); got != "-machine pc" {
		t.Errorf("got %q, want %q", got, "-machine pc")
	}
}
