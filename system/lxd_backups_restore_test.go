package system

import (
	"strings"
	"testing"
)

// A representative VM backup.yaml fragment with a root disk and an attached
// custom-volume disk (disk1) in both devices: and expanded_devices: maps.
const sampleBackupYAML = `config:
  image.os: Ubuntu
container:
  name: lab-vm2-5-3
  devices:
    disk1:
      pool: default
      source: lab-vm2-5-3-disk-1
      type: disk
    net0:
      nictype: bridged
      parent: vmbr0
      type: nic
    root:
      path: /
      pool: default
      type: disk
  expanded_devices:
    disk1:
      pool: default
      source: lab-vm2-5-3-disk-1
      type: disk
    root:
      path: /
      pool: default
      type: disk
snapshots:
- name: snap1
  devices:
    root:
      path: /
      pool: default
      type: disk
pool:
  name: oldpool
  driver: zfs
  config:
    zfs.pool_name: OLD/src
`

func TestBackupYAMLDiskDevices(t *testing.T) {
	devs := backupYAMLDiskDevices(sampleBackupYAML)
	want := map[string]string{"disk1": "lab-vm2-5-3-disk-1", "root": "", "net0-should-not-appear": ""}
	got := map[string]bool{}
	for _, d := range devs {
		got[d.Name] = true
		if d.Name == "disk1" && (d.Source != want["disk1"] || d.Path != "") {
			t.Fatalf("disk1 parsed wrong: %+v", d)
		}
		if d.Name == "root" && d.Path != "/" {
			t.Fatalf("root parsed wrong: %+v", d)
		}
	}
	if !got["disk1"] || !got["root"] {
		t.Fatalf("expected disk1+root disk devices, got %v", got)
	}
	if got["net0"] {
		t.Fatalf("net0 is a nic, must not be a disk device")
	}
}

func TestRewriteStripsUncapturedCustomDisk(t *testing.T) {
	strip := map[string]bool{"disk1": true} // disk1's volume not captured
	out := rewriteBackupYAMLForRestore(sampleBackupYAML, "default", "NVMEPool/LXD-znas5", "lab-vm2-5-3", "lab-vm2-5-3", strip, nil)

	if strings.Contains(out, "lab-vm2-5-3-disk-1") {
		t.Fatalf("disk1 custom volume reference should be stripped:\n%s", out)
	}
	if strings.Count(out, "disk1:") != 0 {
		t.Fatalf("disk1 device block(s) should be removed from both device maps:\n%s", out)
	}
	// Root + nic kept.
	if !strings.Contains(out, "root:") || !strings.Contains(out, "net0:") {
		t.Fatalf("root/net0 devices must be preserved:\n%s", out)
	}
	// snapshots emptied, pool rewritten.
	if !strings.Contains(out, "snapshots: []") {
		t.Fatalf("snapshots not emptied:\n%s", out)
	}
	if strings.Contains(out, "zfs.pool_name: OLD/src") {
		t.Fatalf("zfs.pool_name not rewritten:\n%s", out)
	}
}

func TestRewriteKeepsCapturedCustomDisk(t *testing.T) {
	// No strip set → disk1 retained (e.g. its volume was captured).
	out := rewriteBackupYAMLForRestore(sampleBackupYAML, "default", "NVMEPool/LXD-znas5", "lab-vm2-5-3", "lab-vm2-5-3", nil, nil)
	if !strings.Contains(out, "source: lab-vm2-5-3-disk-1") {
		t.Fatalf("disk1 should be kept when not stripped:\n%s", out)
	}
}

func TestRewriteRemapsCustomDiskSourceOnClone(t *testing.T) {
	// Clone-to-new-name: disk1 kept, but its source remapped to the renamed
	// volume so it can't collide with the source instance's volume.
	remap := map[string]string{"lab-vm2-5-3-disk-1": "clone9-disk-1"}
	out := rewriteBackupYAMLForRestore(sampleBackupYAML, "default", "NVMEPool/LXD-znas5", "lab-vm2-5-3", "clone9", nil, remap)
	if strings.Contains(out, "source: lab-vm2-5-3-disk-1") {
		t.Fatalf("device source should be remapped:\n%s", out)
	}
	if strings.Count(out, "source: clone9-disk-1") != 2 { // devices: + expanded_devices:
		t.Fatalf("expected remapped source in both device maps:\n%s", out)
	}
}
