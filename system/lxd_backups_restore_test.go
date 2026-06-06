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

// A backup.yaml with a host-path cdrom (source begins "/") and a custom data
// disk on a DIFFERENT pool than the root — the exact shape of fresh-2604srv.
const sampleBackupYAMLCdromXpool = `container:
  name: fresh-2604srv
  devices:
    cdrom0:
      boot.priority: "10"
      readonly: "true"
      source: /BIGRAID5/.isos/ubuntu-26.04.iso
      type: disk
    data1:
      pool: BigRaid5
      source: fresh-2604srv-data1
      type: disk
    root:
      path: /
      pool: default
      type: disk
  expanded_devices:
    cdrom0:
      source: /BIGRAID5/.isos/ubuntu-26.04.iso
      type: disk
    data1:
      pool: BigRaid5
      source: fresh-2604srv-data1
      type: disk
    root:
      path: /
      pool: default
      type: disk
snapshots: []
`

func TestRestoreStripDevsHostPathAndUncaptured(t *testing.T) {
	// data1's volume IS captured (in volRemap); cdrom0 is a host path → stripped.
	remap := map[string]string{"fresh-2604srv-data1": "fresh-2604srv-rt-data1"}
	strip := restoreStripDevs(sampleBackupYAMLCdromXpool, remap)
	if !strip["cdrom0"] {
		t.Errorf("host-path cdrom0 must be stripped, got %v", strip)
	}
	if strip["data1"] {
		t.Errorf("captured data1 must NOT be stripped, got %v", strip)
	}
	if strip["root"] {
		t.Errorf("root disk must never be stripped, got %v", strip)
	}
	// nil volRemap → keep everything (legacy callers).
	if len(restoreStripDevs(sampleBackupYAMLCdromXpool, nil)) != 0 {
		t.Errorf("nil volRemap should strip nothing")
	}
}

func TestRewriteStripsCdromAndRewritesDevicePool(t *testing.T) {
	remap := map[string]string{"fresh-2604srv-data1": "fresh-2604srv-rt-data1"}
	strip := restoreStripDevs(sampleBackupYAMLCdromXpool, remap)
	out := rewriteBackupYAMLForRestore(sampleBackupYAMLCdromXpool, "default", "nvmepool/LXD-zn216",
		"fresh-2604srv", "fresh-2604srv-rt", strip, remap)
	if strings.Contains(out, "cdrom0:") || strings.Contains(out, "/BIGRAID5/.isos") {
		t.Errorf("cdrom0 host-path device should be gone:\n%s", out)
	}
	if strings.Contains(out, "pool: BigRaid5") {
		t.Errorf("device-level pool must be rewritten to dstPool 'default':\n%s", out)
	}
	if !strings.Contains(out, "source: fresh-2604srv-rt-data1") {
		t.Errorf("data1 source should be remapped to the restored volume:\n%s", out)
	}
	if !strings.Contains(out, "data1:") {
		t.Errorf("captured data1 device must be kept:\n%s", out)
	}
}
