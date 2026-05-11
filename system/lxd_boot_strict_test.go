package system

import (
	"strings"
	"testing"
)

// TestSetBootStrictOffEmptyInput covers the fresh-VM case where raw.qemu has
// not been set yet. Regression test for the "no OS to boot from" symptom on
// Debian 13 / Ubuntu 26.04: without this clause OVMF halted at the empty
// root disk and never iterated to the CDROM.
func TestSetBootStrictOffEmptyInput(t *testing.T) {
	got := setBootStrictOff("")
	want := "-boot order=dcn,strict=off"
	if got != want {
		t.Errorf("setBootStrictOff(\"\") = %q, want %q", got, want)
	}
}

// TestSetBootStrictOffNoMenuOn — regression test for the v6.5.10 production
// incident. With menu=on, OVMF can sit on its boot-manager screen when no
// device boots and stop servicing QMP, which hangs Incus and (transitively)
// blocks zfsnas startup. We must NEVER inject menu=on here.
func TestSetBootStrictOffNoMenuOn(t *testing.T) {
	for _, in := range []string{"", "-smp sockets=2", "-boot strict=on -smp sockets=2"} {
		got := setBootStrictOff(in)
		if strings.Contains(got, "menu=on") {
			t.Errorf("setBootStrictOff(%q) leaked menu=on: %q", in, got)
		}
	}
}

// TestSetBootStrictOffPreservesUnrelated checks that other raw.qemu content
// (sockets, SMBIOS, machine type, CDROMs themselves) is preserved when we
// inject -boot strict=off.
func TestSetBootStrictOffPreservesUnrelated(t *testing.T) {
	in := "-smp sockets=2 -smbios type=1,serial=ABC -machine q35"
	got := setBootStrictOff(in)
	if !strings.Contains(got, "-smp sockets=2") {
		t.Errorf("dropped -smp clause: %q", got)
	}
	if !strings.Contains(got, "-smbios type=1,serial=ABC") {
		t.Errorf("dropped -smbios clause: %q", got)
	}
	if !strings.Contains(got, "-machine q35") {
		t.Errorf("dropped -machine clause: %q", got)
	}
	if !strings.Contains(got, "strict=off") {
		t.Errorf("missing strict=off in: %q", got)
	}
}

// TestSetBootStrictOffReplacesPriorBoot verifies that an existing -boot
// clause is rewritten in place (no duplicate -boot args left behind that
// QEMU might collapse unpredictably).
func TestSetBootStrictOffReplacesPriorBoot(t *testing.T) {
	in := "-boot strict=on -smp sockets=2"
	got := setBootStrictOff(in)
	if strings.Count(got, "-boot ") != 1 {
		t.Errorf("expected exactly one -boot clause, got %q", got)
	}
	if !strings.Contains(got, "strict=off") {
		t.Errorf("missing strict=off clause in: %q", got)
	}
	if strings.Contains(got, "strict=on") {
		t.Errorf("stale strict=on left behind in: %q", got)
	}
}

// TestSetBootStrictOffIsIdempotent — calling twice should leave a single
// -boot clause. Important because vmApplyCDROMs runs on every VM edit, not
// just creation.
func TestSetBootStrictOffIsIdempotent(t *testing.T) {
	once := setBootStrictOff("-smp sockets=2")
	twice := setBootStrictOff(once)
	if strings.Count(twice, "-boot ") != 1 {
		t.Errorf("re-applying setBootStrictOff produced multiple -boot clauses: %q", twice)
	}
	if once != twice {
		t.Errorf("not idempotent — once=%q, twice=%q", once, twice)
	}
}

// TestSetCDROMsRawQEMUStillUsesBootindexBase guards the contract between the
// CDROM emitter and OVMF: each CDROM must carry an explicit bootindex >=
// cdromBootindexBase (10) so it doesn't collide with Incus' auto-assigned
// slots for the root disk and eth0. If someone bumps the constant, the
// tests should follow.
func TestSetCDROMsRawQEMUStillUsesBootindexBase(t *testing.T) {
	got := setCDROMsRawQEMU("", []string{"/tank/.isos/test.iso"})
	if !strings.Contains(got, "bootindex=10") {
		t.Errorf("expected bootindex=10 (cdromBootindexBase) in: %q", got)
	}
	if !strings.Contains(got, "bus=ide.0") {
		t.Errorf("expected bus=ide.0 (Q35 ICH9 AHCI port 0) in: %q", got)
	}
	if !strings.Contains(got, "media=cdrom") {
		t.Errorf("expected media=cdrom in: %q", got)
	}
}
