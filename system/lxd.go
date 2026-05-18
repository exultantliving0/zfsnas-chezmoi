package system

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// lxdNormalizeSizeStr converts a raw-bytes LXD size value (e.g. "20000008192B"
// — what we pass to `lxc init` after rounding for ZFS volblocksize alignment)
// into a friendlier suffix form like "20GB" so the Edit Disks UI doesn't
// render the long byte literal in the size input.
//
// Strings that already use a friendly suffix (KB/MB/GB/TB or KiB/MiB/GiB/TiB)
// are passed through unchanged. The bare-bytes form is rounded to the unit
// where the value is closest to a whole number within 1% — that matches the
// user's original intent (e.g. they asked for 20 GB; the +8 192 byte ZFS
// alignment delta is well within tolerance and rounds back to "20GB").
func lxdNormalizeSizeStr(s string) string {
	if s == "" {
		return s
	}
	// Already friendly? Anything ending in two letters (B/iB/etc.) passes
	// through untouched. Bare bytes is exactly digits + final 'B'.
	if !strings.HasSuffix(s, "B") {
		return s
	}
	stripped := strings.TrimSuffix(s, "B")
	if stripped == "" {
		return s
	}
	// If the char before 'B' is a letter, this isn't bare bytes (it's KB,
	// MB, GiB, etc.) — leave it alone.
	last := stripped[len(stripped)-1]
	if last < '0' || last > '9' {
		return s
	}
	bytes, err := strconv.ParseInt(stripped, 10, 64)
	if err != nil || bytes <= 0 {
		return s
	}
	type unit struct {
		suffix string
		scale  float64
	}
	units := []unit{
		{"TB", 1e12}, {"TiB", 1 << 40},
		{"GB", 1e9}, {"GiB", 1 << 30},
		{"MB", 1e6}, {"MiB", 1 << 20},
		{"KB", 1e3}, {"KiB", 1 << 10},
	}
	for _, u := range units {
		v := float64(bytes) / u.scale
		r := math.Round(v)
		if r < 1 {
			continue
		}
		if math.Abs(v-r)/r <= 0.01 {
			return fmt.Sprintf("%d%s", int64(r), u.suffix)
		}
	}
	return s
}

// LXDInstance represents a LXD virtual machine or container.
type LXDInstance struct {
	Name        string `json:"name"`
	Description string `json:"description"` // human-readable display name
	Type        string `json:"type"`        // "virtual-machine" | "container"
	Status      string `json:"status"`      // "Running", "Stopped", "Starting", "Stopping", ...
	IPv4        string `json:"ipv4"`
	Image       string `json:"image"`
	CPULimit    string `json:"cpu_limit"`
	MemoryLimit string `json:"memory_limit"`
	RootPool    string `json:"root_pool"` // LXD storage pool name for the root disk
	Autostart   bool   `json:"autostart"` // boot.autostart=true on the instance or its profile
}

// LXDImage is an image available for instance creation.
type LXDImage struct {
	Fingerprint string `json:"fingerprint"`
	Description string `json:"description"`
	OS          string `json:"os"`
	Version     string `json:"version"`
	Arch        string `json:"arch"`
	Type        string `json:"type"` // "virtual-machine" | "container"
	Size        int64  `json:"size"`
	Variant     string `json:"variant"`
	Serial      string `json:"serial"`
}

// LXDDisk is an extra virtual disk for a VM or container.
// SizeGB is float so the unit selector (MB/GB/TB) can express fractional GB
// (e.g. 100MB → 0.1).
//
// IncludeInSnapshots (default true on the wire) tags the underlying custom
// volume with `user.znas:snap_with_instance=true` so the snapshot/restore
// path knows to take a matching volume snapshot every time the instance
// is snapshotted. With it false, the volume is independent — instance
// snapshots leave the disk's data untouched.
type LXDDisk struct {
	DeviceName         string  `json:"device_name"`
	Pool               string  `json:"pool"`
	SizeGB             float64 `json:"size_gb"`
	ReservePct         int     `json:"reserve_pct"` // 0=thin, 25/50/75/100
	IncludeInSnapshots bool    `json:"include_in_snapshots"`
}

// LXDSnapWithInstanceProperty is the user property ZNAS sets on a custom
// storage volume to mark it as "follow the instance's snapshot lifecycle".
// CreateLXDSnapshot scans every attached disk device, looks up its source
// volume's user properties, and snapshots/restores in lockstep when the
// property is "true". Stored under `user.znas:` so it doesn't collide with
// any Incus internal config keys.
const LXDSnapWithInstanceProperty = "user.znas:snap_with_instance"

// LXDExistingDisk references an existing ZFS volume to attach as a raw block device.
type LXDExistingDisk struct {
	DeviceName string `json:"device_name"`
	DevPath    string `json:"dev_path"` // /dev/zvol/<pool>/<name>
}

// LXDFreeZVol is a volume available for attachment.
// Raw ZVols: DevPath = "/dev/zvol/…"
// LXD-managed volumes: DevPath = "lxd:<pool>/<volname>"
type LXDFreeZVol struct {
	Name    string  `json:"name"`     // display name
	DevPath string  `json:"dev_path"` // /dev/zvol/… or lxd:<pool>/<vol>
	SizeGB  float64 `json:"size_gb"`
}

// ListFreeZVols returns ZFS volumes not managed by any LXD storage pool.
func ListFreeZVols() ([]LXDFreeZVol, error) {
	zvols, err := ListAllZVols()
	if err != nil {
		return nil, err
	}
	// Collect ZFS dataset prefixes used by LXD storage pools (zfs driver only).
	excludePrefixes := lxdPoolZFSPrefixes()

	var out []LXDFreeZVol
	for _, zv := range zvols {
		managed := false
		for _, pfx := range excludePrefixes {
			if strings.HasPrefix(zv.Name, pfx+"/") || zv.Name == pfx {
				managed = true
				break
			}
		}
		if !managed {
			out = append(out, LXDFreeZVol{
				Name:    zv.Name,
				DevPath: zv.DevPath,
				SizeGB:  float64(zv.Size) / (1 << 30),
			})
		}
	}

	// Also include unattached LXD-managed custom volumes (e.g. detached disks).
	out = append(out, listFreeLXDManagedVols()...)

	if out == nil {
		out = []LXDFreeZVol{}
	}
	return out, nil
}

// listFreeLXDManagedVols returns custom volumes inside LXD storage pools that are
// not currently attached to any instance (i.e. detached disks).
func listFreeLXDManagedVols() []LXDFreeZVol {
	// Get all pools
	poolsOut, err := exec.Command("incus", "query", "/1.0/storage-pools?recursion=1").Output()
	if err != nil {
		return nil
	}
	var pools []struct {
		Name   string            `json:"name"`
		Driver string            `json:"driver"`
		Config map[string]string `json:"config"`
	}
	if json.Unmarshal(poolsOut, &pools) != nil {
		return nil
	}

	var out []LXDFreeZVol
	for _, pool := range pools {
		volsOut, err := exec.Command("incus", "query",
			"/1.0/storage-pools/"+pool.Name+"/volumes/custom?recursion=1").Output()
		if err != nil {
			continue
		}
		var vols []struct {
			Name   string   `json:"name"`
			UsedBy []string `json:"used_by"`
			Config struct {
				Size string `json:"size"`
			} `json:"config"`
		}
		if json.Unmarshal(volsOut, &vols) != nil {
			continue
		}
		for _, v := range vols {
			if len(v.UsedBy) > 0 {
				continue // still attached
			}
			// Verify the backing ZFS volume actually exists — LXD can have
			// stale entries in its database after volumes are deleted directly.
			if lxdFindZFSVol(v.Name) == "" {
				// Prune the orphaned LXD inventory entry.
				exec.Command("incus", "storage", "volume", "delete", pool.Name, v.Name).Run()
				continue
			}
			sizeGB := parseVolSizeGB(v.Config.Size)
			out = append(out, LXDFreeZVol{
				Name:    v.Name + " (" + pool.Name + ")",
				DevPath: "lxd:" + pool.Name + "/" + v.Name,
				SizeGB:  sizeGB,
			})
		}
	}
	return out
}

// lxdVolSizeBytes parses an LXD size string (e.g. "20GiB", "10GB") into bytes.
func lxdVolSizeBytes(s string) int64 {
	s = strings.TrimSpace(s)
	units := []struct {
		suffix string
		mult   int64
	}{
		{"TiB", 1 << 40}, {"TB", 1e12},
		{"GiB", 1 << 30}, {"GB", 1e9},
		{"MiB", 1 << 20}, {"MB", 1e6},
		{"KiB", 1 << 10}, {"KB", 1e3},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			n, err := strconv.ParseFloat(strings.TrimSuffix(s, u.suffix), 64)
			if err == nil {
				return int64(n * float64(u.mult))
			}
		}
	}
	// Bare-bytes form, e.g. "20000008192B" — what we now pass to `lxc init` to
	// land the volsize on a 16K ZFS boundary. The unit loop above doesn't
	// match it because no K/M/G/T prefix is present.
	if strings.HasSuffix(s, "B") {
		if n, err := strconv.ParseInt(strings.TrimSuffix(s, "B"), 10, 64); err == nil {
			return n
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err == nil {
		return n
	}
	return 0
}

// parseLXDVolRef parses a "lxd:<pool>/<vol>" reference into (pool, vol, true).
// Returns ("", "", false) for anything else (raw /dev/zvol paths, empty strings, etc.).
func parseLXDVolRef(s string) (pool, vol string, ok bool) {
	if !strings.HasPrefix(s, "lxd:") {
		return "", "", false
	}
	rest := strings.TrimPrefix(s, "lxd:")
	slash := strings.Index(rest, "/")
	if slash <= 0 {
		return "", "", false
	}
	return rest[:slash], rest[slash+1:], true
}

// parseVolSizeGB parses LXD size strings like "10GiB", "20GB", "512MiB" into GB.
func parseVolSizeGB(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	units := []struct {
		suffix string
		factor float64
	}{
		{"GiB", 1.0}, {"GB", 1.0},
		{"MiB", 1.0 / 1024}, {"MB", 1.0 / 1000},
		{"TiB", 1024.0}, {"TB", 1000.0},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			n, err := strconv.ParseFloat(strings.TrimSuffix(s, u.suffix), 64)
			if err == nil {
				return n * u.factor
			}
		}
	}
	// Plain number — assume bytes
	n, err := strconv.ParseFloat(s, 64)
	if err == nil {
		return n / (1 << 30)
	}
	return 0
}

// lxdPoolZFSPrefixes returns the ZFS dataset paths used by LXD ZFS-backed storage pools.
func lxdPoolZFSPrefixes() []string {
	out, err := exec.Command("incus", "query", "/1.0/storage-pools?recursion=1").Output()
	if err != nil {
		return nil
	}
	var pools []struct {
		Driver string            `json:"driver"`
		Config map[string]string `json:"config"`
	}
	if json.Unmarshal(out, &pools) != nil {
		return nil
	}
	var prefixes []string
	for _, p := range pools {
		if p.Driver != "zfs" {
			continue
		}
		ds := p.Config["zfs.pool_name"]
		if ds == "" {
			ds = p.Config["source"]
		}
		if ds != "" {
			prefixes = append(prefixes, ds)
		}
	}
	return prefixes
}

// LXDNIC is a network interface for an instance.
type LXDNIC struct {
	DeviceName string `json:"device_name"`
	Network    string `json:"network"`
	MAC        string `json:"mac"`
	Connected  bool   `json:"connected"` // false → remove NIC device from instance config
	VlanID     int    `json:"vlan_id,omitempty"`
	IPv4Mode   string `json:"ipv4_mode,omitempty"` // "dhcp" | "static" | "none"
	IPv4Addr   string `json:"ipv4_addr,omitempty"` // e.g. "10.0.0.10/24"
	IPv4GW     string `json:"ipv4_gw,omitempty"`   // gateway IP
	DNS1       string `json:"dns1,omitempty"`      // primary DNS (static mode only)
	DNS2       string `json:"dns2,omitempty"`      // secondary DNS (static mode only)
}

// LXDUSBDevice is a USB device to pass through to a VM.
type LXDUSBDevice struct {
	DeviceName string `json:"device_name"`
	VendorID   string `json:"vendor_id"`
	ProductID  string `json:"product_id"`
	Desc       string `json:"desc"`
}

// LXDPCIDevice is a PCI device to pass through to a VM.
type LXDPCIDevice struct {
	DeviceName   string `json:"device_name"`
	Address      string `json:"address"` // e.g. "0000:02:00.0"
	Desc         string `json:"desc"`
	ROMBar       string `json:"rombar,omitempty"`         // "0" or "1"; "" = LXD default
	AER          string `json:"aer,omitempty"`            // "0" or "1"; "" = LXD default
	XVGA         string `json:"x_vga,omitempty"`          // "0" or "1"; "" = LXD default
	XIGDOpRegion string `json:"x_igd_opregion,omitempty"` // "0" or "1"; "" = unset. Intel iGPU OpRegion ACPI buffer; required for i915 firmware/power features under passthrough.
	XIGDGMS      string `json:"x_igd_gms,omitempty"`      // "0".."16"; "" = unset. Intel iGPU stolen-memory size (multiples of 32MB; 2 = 64MB, the value Plex VAAPI transcoding needs).
}

// LXDPassthroughDevice is a generic device passthrough for containers.
type LXDPassthroughDevice struct {
	DeviceName string            `json:"device_name"`
	Type       string            `json:"type"` // unix-char, unix-block, usb, gpu, disk
	HostPath   string            `json:"host_path"`
	Extra      map[string]string `json:"extra"`
}

// LXDCreateVMRequest contains all parameters for VM creation.
type LXDCreateVMRequest struct {
	Name              string            `json:"name"`
	Description       string            `json:"description"`
	Image             string            `json:"image"`
	Profile           string            `json:"profile"`
	AutoStart         bool              `json:"auto_start"`
	ForceRunning      bool              `json:"force_running"`
	VCPU              int               `json:"vcpu"`
	MemoryMB          int               `json:"memory_mb"`
	MemoryHugepages   bool              `json:"memory_hugepages"`
	RootPool          string            `json:"root_pool"`
	RootSizeGB        float64           `json:"root_size_gb"`
	ExtraDisks        []LXDDisk         `json:"extra_disks"`
	ExistingDisks     []LXDExistingDisk `json:"existing_disks_raw"`
	NICs              []LXDNIC          `json:"nics"`
	USBDevices        []LXDUSBDevice    `json:"usb_devices"`
	PCIDevices        []LXDPCIDevice    `json:"pci_devices"`
	CloudInit         string            `json:"cloud_init"`
	CDROMPath         string            `json:"cdrom_path"`         // absolute path to ISO, "" = no disc
	CDROMPool         string            `json:"cdrom_pool"`         // pool name — handler resolves to CDROMPath
	CDROMIso          string            `json:"cdrom_iso"`          // ISO filename within pool's .isos dir
	CDROMs            []string          `json:"cdroms"`             // handler-resolved absolute ISO paths (multi-drive)
	CPUSockets        int               `json:"cpu_sockets"`        // QEMU socket topology (0 = auto)
	CPUPin            string            `json:"cpu_pin"`            // LXD limits.cpu range string for pinning
	StatefulSnapshots bool              `json:"stateful_snapshots"` // sets migration.stateful before first start
	Firmware          string            `json:"firmware"`           // "uefi" (default) | "bios"
	SecureBoot        bool              `json:"secure_boot"`        // only meaningful when Firmware == "uefi"
	TPM               bool              `json:"tpm"`                // enable emulated TPM 2.0 (security.tpm)
	MachineType       string            `json:"machine_type"`       // "" = auto, "pc-q35-9.1", "pc-i440fx-9.1", "q35", "pc", etc.
	DiskBus           string            `json:"disk_bus"`           // "" = virtio-blk (default), "scsi", "nvme"
	SMBIOS            *LXDSMBIOSType1   `json:"smbios,omitempty"`   // SMBIOS type 1 (System Information); applied via raw.qemu
	SMBIOSType2       *LXDSMBIOSType2   `json:"smbios_type2,omitempty"` // SMBIOS type 2 (Baseboard / Motherboard); applied via raw.qemu
	SMBIOSType4       *LXDSMBIOSType4   `json:"smbios_type4,omitempty"` // SMBIOS type 4 (Processor); applied via raw.qemu
	DisableVirtualVGA bool              `json:"disable_virtual_vga"` // replace Incus' default virtio-vga with a passive bridge; used for full GPU passthrough so the guest doesn't bind a framebuffer console
}

// LXDSMBIOSType1 holds the seven fields exposed by Proxmox under "SMBIOS
// settings (type1)". Mirrors QEMU's `-smbios type=1,…` clause. All fields
// optional — empty values are omitted from the QEMU command line so the
// firmware default applies.
type LXDSMBIOSType1 struct {
	UUID         string `json:"uuid,omitempty"`
	Manufacturer string `json:"manufacturer,omitempty"`
	Product      string `json:"product,omitempty"`
	Version      string `json:"version,omitempty"`
	Serial       string `json:"serial,omitempty"`
	SKU          string `json:"sku,omitempty"`
	Family       string `json:"family,omitempty"`
}

// LXDSMBIOSType2 holds the six fields QEMU exposes for SMBIOS type 2
// (Baseboard / Motherboard Information). Mirrors `-smbios type=2,…`.
type LXDSMBIOSType2 struct {
	Manufacturer string `json:"manufacturer,omitempty"`
	Product      string `json:"product,omitempty"`
	Version      string `json:"version,omitempty"`
	Serial       string `json:"serial,omitempty"`
	Asset        string `json:"asset,omitempty"`
	Location     string `json:"location,omitempty"`
}

// LXDSMBIOSType4 holds the fields QEMU exposes for SMBIOS type 4 (Processor
// Information). Mirrors `-smbios type=4,…`. Numeric fields use 0 to mean
// "omit"; QEMU then falls back to its built-in defaults.
type LXDSMBIOSType4 struct {
	SockPfx         string `json:"sock_pfx,omitempty"`
	Manufacturer    string `json:"manufacturer,omitempty"`
	Version         string `json:"version,omitempty"`
	Serial          string `json:"serial,omitempty"`
	Asset           string `json:"asset,omitempty"`
	Part            string `json:"part,omitempty"`
	ProcessorFamily int    `json:"processor_family,omitempty"` // SMBIOS processor family code (e.g. 0xB3 = Intel Xeon)
	MaxSpeed        int    `json:"max_speed,omitempty"`        // MHz
	CurrentSpeed    int    `json:"current_speed,omitempty"`    // MHz
}

// LXDCreateContainerRequest contains all parameters for container creation.
type LXDCreateContainerRequest struct {
	Name          string                 `json:"name"`
	Description   string                 `json:"description"`
	Image         string                 `json:"image"`
	Profile       string                 `json:"profile"`
	AutoStart     bool                   `json:"auto_start"`
	ForceRunning  bool                   `json:"force_running"`
	CPUCores      int                    `json:"cpu_cores"`
	CPUShares     int                    `json:"cpu_shares"`    // 1-10, maps to limits.cpu.priority
	CPULimitPct   int                    `json:"cpu_limit_pct"` // 0=unlimited; 1-100 → limits.cpu.allowance
	MemoryMB      int                    `json:"memory_mb"`
	SwapMB        int                    `json:"swap_mb"` // -1 = no swap, 0 = unlimited, >0 = N MB
	DiskSizeGB    float64                `json:"disk_size_gb"`
	RootPool      string                 `json:"root_pool"`
	Unprivileged  bool                   `json:"unprivileged"`   // true = security.privileged=false (default)
	Nesting       bool                   `json:"nesting"`        // security.nesting=true
	FeatureKeyctl bool                   `json:"feature_keyctl"` // security.syscalls.allow=keyctl
	FeatureFUSE   bool                   `json:"feature_fuse"`   // adds /dev/fuse device
	RootPassword  string                 `json:"root_password"`  // set via chpasswd after start
	StartupOrder  int                    `json:"startup_order"`
	StartupDelay  int                    `json:"startup_delay"`
	Devices       []LXDPassthroughDevice `json:"devices"`
	NICs          []LXDNIC               `json:"nics"`
	// Optional additional storage. Containers attach plain disks (no
	// "block" type — that's VM-only); SizeGB applies as a quota on the
	// pool-managed volume. Supports fractional GB so the UI can offer an
	// MB unit.
	ExtraDisks    []LXDDisk         `json:"extra_disks"`
	ExistingDisks []LXDExistingDisk `json:"existing_disks"`
}

// LXDInstanceStats holds live resource usage for a running instance.
type LXDInstanceStats struct {
	Status        string  `json:"status"`
	UptimeSec     int64   `json:"uptime_sec"` // seconds since instance started (0 if unknown)
	CPUUsageNs    int64   `json:"cpu_usage_ns"`
	CPUPct        float64 `json:"cpu_pct"`   // current CPU % across all vCPUs (0-100)
	CPUCount      int     `json:"cpu_count"` // number of vCPUs configured
	MemUsedBytes  int64   `json:"mem_used_bytes"`
	MemPeakBytes  int64   `json:"mem_peak_bytes"`
	MemLimitBytes int64   `json:"mem_limit_bytes"` // 0 = unlimited
	// Guest-reported MemTotal from the Incus Prometheus endpoint. For VMs
	// this is the virtio-balloon driver's reported total; non-zero means
	// the balloon driver is loaded and reporting stats. For containers
	// this is the cgroup memory total — the frontend only renders this in
	// VM context where it is meaningful.
	BalloonCurrentBytes int64 `json:"balloon_current_bytes,omitempty"`
	// Sum across ALL disk devices on the instance — not just the root disk.
	// Includes attached zvols / extra disks. The synthetic Incus "agent"
	// share is excluded.
	DiskUsedBytes int64 `json:"disk_used_bytes"`
	DiskSizeBytes int64 `json:"disk_size_bytes"` // 0 = unlimited / unknown
	// Weighted-by-configured-size average ZFS compressratio across all
	// disks (e.g. "1.34x"). Empty when no disk reports a ratio.
	DiskAvgCompRatio string `json:"disk_avg_comp_ratio,omitempty"`
	Processes        int    `json:"processes"`
}

// parseLXDBytes converts LXD size strings like "4GB", "2GiB", "512MB" to bytes.
func parseLXDBytes(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	units := []struct {
		suffix string
		factor int64
	}{
		{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10},
		{"GB", 1_000_000_000}, {"MB", 1_000_000}, {"KB", 1_000},
		{"G", 1 << 30}, {"M", 1 << 20}, {"K", 1 << 10},
	}
	for _, u := range units {
		if strings.HasSuffix(s, u.suffix) {
			var v float64
			fmt.Sscanf(strings.TrimSuffix(s, u.suffix), "%f", &v)
			return int64(v * float64(u.factor))
		}
	}
	var v int64
	fmt.Sscanf(s, "%d", &v)
	return v
}

// LXDGetInstanceStats fetches live CPU/memory/disk usage for one instance.
// It performs two CPU samples 300 ms apart to compute a live CPU-usage percentage.
func LXDGetInstanceStats(name string) (LXDInstanceStats, error) {
	if !lxdNameRe.MatchString(name) {
		return LXDInstanceStats{}, fmt.Errorf("invalid instance name")
	}

	type stateRaw struct {
		Status    string `json:"status"`
		StartedAt string `json:"started_at"` // RFC3339; only present in LXD 5.2+ (api ext instance_state_started_at)
		Pid       int64  `json:"pid"`        // host PID of the container init / VM process
		CPU       struct {
			Usage int64 `json:"usage"`
		} `json:"cpu"`
		Memory struct {
			Usage     int64 `json:"usage"`
			UsagePeak int64 `json:"usage_peak"`
		} `json:"memory"`
		Disk map[string]struct {
			Usage int64 `json:"usage"`
		} `json:"disk"`
		Processes int `json:"processes"`
	}
	queryState := func() (int64, *stateRaw, error) {
		out, err := exec.Command("incus", "query", "/1.0/instances/"+name+"/state").Output()
		if err != nil {
			return 0, nil, err
		}
		var raw stateRaw
		if err := json.Unmarshal(out, &raw); err != nil {
			return 0, nil, err
		}
		return raw.CPU.Usage, &raw, nil
	}

	t1 := time.Now()
	cpu1, raw1, err := queryState()
	if err != nil {
		return LXDInstanceStats{}, fmt.Errorf("query state: %w", err)
	}

	// Non-running instances: return immediately without a second sample.
	if raw1.Status != "Running" {
		return LXDInstanceStats{Status: raw1.Status}, nil
	}

	time.Sleep(300 * time.Millisecond)
	t2 := time.Now()
	cpu2, raw2, err := queryState()
	if err != nil {
		// Fall back to first sample if second fails.
		raw2 = raw1
		cpu2 = cpu1
	}

	elapsedNs := t2.Sub(t1).Nanoseconds()
	cpuDelta := cpu2 - cpu1

	// Read config to get limits.
	cfg, _ := LXDGetConfig(name)

	cpuCount := 1
	if cfg.CPULimit != "" {
		// limits.cpu can be "2" or a range like "0-3"
		if strings.Contains(cfg.CPULimit, "-") {
			var lo, hi int
			fmt.Sscanf(cfg.CPULimit, "%d-%d", &lo, &hi)
			cpuCount = hi - lo + 1
		} else {
			fmt.Sscanf(cfg.CPULimit, "%d", &cpuCount)
		}
		if cpuCount < 1 {
			cpuCount = 1
		}
	}

	var cpuPct float64
	if elapsedNs > 0 {
		cpuPct = float64(cpuDelta) / float64(elapsedNs) / float64(cpuCount) * 100.0
		if cpuPct < 0 {
			cpuPct = 0
		}
		if cpuPct > 100 {
			cpuPct = 100
		}
	}

	memLimit := parseLXDBytes(cfg.MemoryLimit)

	// Aggregate disk size + weighted compressratio across ALL disks on
	// the instance, not just the root. Excludes the synthetic Incus agent
	// share (source=agent:config). The weighted-average ratio uses each
	// disk's configured size as the weight, so a tiny scratch zvol with
	// an outlier ratio doesn't skew the headline number for a 200 GB root.
	diskSize := int64(0)
	var ratioWeighted float64
	var ratioWeight int64
	for _, d := range cfg.Disks {
		if d.IsAgent {
			continue
		}
		s := parseLXDBytes(d.Size)
		if s <= 0 {
			continue
		}
		diskSize += s
		r := parseCompRatioFloat(d.CompRatio)
		if r > 0 {
			ratioWeighted += float64(s) * r
			ratioWeight += s
		}
	}
	avgCompRatio := ""
	if ratioWeight > 0 {
		avgCompRatio = fmt.Sprintf("%.2fx", ratioWeighted/float64(ratioWeight))
	}

	// Incus's instance/state JSON only reports the root disk's usage
	// under raw2.Disk["root"] — attached zvols and extra disks are NOT
	// listed there, so summing raw2.Disk grossly under-counts on
	// multi-disk instances. Query the actual `used` value from ZFS
	// directly for every non-agent disk in one batched `zfs get` call
	// and sum those. Fall back to raw2.Disk only when no ZFSPath is
	// known (non-ZFS storage backend).
	var zfsPaths []string
	for _, d := range cfg.Disks {
		if d.IsAgent || d.ZFSPath == "" {
			continue
		}
		zfsPaths = append(zfsPaths, d.ZFSPath)
	}
	usedByPath := zfsGetUsedForPaths(zfsPaths)
	disk := int64(0)
	for _, d := range cfg.Disks {
		if d.IsAgent || d.ZFSPath == "" {
			continue
		}
		disk += usedByPath[d.ZFSPath]
	}
	if disk == 0 {
		for _, d := range raw2.Disk {
			if d.Usage > 0 {
				disk += d.Usage
			}
		}
	}

	var uptimeSec int64
	// Try started_at (LXD 5.2+ with api ext instance_state_started_at).
	if raw2.StartedAt != "" {
		if t, err := time.Parse(time.RFC3339, raw2.StartedAt); err == nil && t.Year() > 2000 {
			uptimeSec = int64(time.Since(t).Seconds())
			if uptimeSec < 0 {
				uptimeSec = 0
			}
		}
	}
	// Fall back to reading /proc/{pid}/stat — works on all LXD versions.
	if uptimeSec == 0 && raw2.Pid > 0 {
		uptimeSec = pidUptimeSec(raw2.Pid)
	}

	memUsed := raw2.Memory.Usage
	memPeak := raw2.Memory.UsagePeak
	// VMs without lxd-agent: the /state endpoint reports memory.usage = 0
	// and cpu.usage stuck at 0 because the cgroup doesn't see inside the
	// QEMU process. LXD's prom endpoint reports real values for both via
	// QEMU's balloon driver and the host-side qemu cgroup, so fall back
	// to it whenever /state has nothing useful. The Monitor tab's
	// realtime row uses the same source via GetLXDInstanceRealtime;
	// piggy-backing on its rate cache here means the Live Info card,
	// when it polls every 3 s, reuses whatever baseline the realtime row
	// last established.
	// Always sample the prom endpoint — it gives us the guest-reported
	// MemTotal (= virtio-balloon current size for VMs) in addition to the
	// memUsed fallback we already use for agent-less VMs.
	promUsed, promTotal := lxdPromMemoryFor(name)
	if memUsed == 0 && promUsed > 0 {
		memUsed = promUsed
		if memLimit == 0 && promTotal > 0 {
			memLimit = promTotal
		}
	}
	if rt, rerr := GetLXDInstanceRealtime(name); rerr == nil && rt != nil {
		if rt.CPUPct != nil {
			// Prefer the prom-derived rate. Equally accurate for
			// containers and the only working source for VMs without
			// lxd-agent. nil here means "no previous baseline yet" —
			// keep the /state-derived cpuPct (0 for agent-less VMs).
			cpuPct = *rt.CPUPct
		}
		// Memory: realtime returns the same MemTotal − MemAvailable
		// derivation, so it's the freshest reading even on the
		// non-zero-/state path. Use it when it's larger, which avoids
		// underreporting when /state's value is stale.
		if rt.MemUsed > 0 && rt.MemUsed > memUsed {
			memUsed = rt.MemUsed
		}
		if memLimit == 0 && rt.MemTotal > 0 {
			memLimit = rt.MemTotal
		}
	}

	// CPU cumulative time: /state reports cpu.usage=0 for VMs without
	// lxd-agent (same reason memory falls back to prom above). Pull from
	// the Prometheus endpoint instead — lxd_cpu_seconds_total summed across
	// every busy mode is the canonical cumulative figure.
	cpuUsageNs := raw2.CPU.Usage
	if cpuUsageNs == 0 {
		if v := lxdPromCPUFor(name); v > 0 {
			cpuUsageNs = v
		}
	}

	return LXDInstanceStats{
		Status:              raw2.Status,
		UptimeSec:           uptimeSec,
		CPUUsageNs:          cpuUsageNs,
		CPUPct:              cpuPct,
		CPUCount:            cpuCount,
		MemUsedBytes:        memUsed,
		MemPeakBytes:        memPeak,
		MemLimitBytes:       memLimit,
		BalloonCurrentBytes: promTotal,
		DiskUsedBytes:       disk,
		DiskSizeBytes:       diskSize,
		DiskAvgCompRatio:    avgCompRatio,
		Processes:           raw2.Processes,
	}, nil
}

// parseCompRatioFloat turns a ZFS compressratio string like "1.23x" (or
// "1.23") into a float. Returns 0 when the value can't be parsed or is
// <= 1 (no compression).
func parseCompRatioFloat(s string) float64 {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "x"))
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v <= 0 {
		return 0
	}
	return v
}

// lxdPromCPUFor scrapes the LXD/Incus Prometheus endpoint and returns the
// cumulative busy CPU time for the named instance, in nanoseconds. Sums
// `lxd_cpu_seconds_total` across every mode except idle and steal (those
// grow ~1 s/s per vCPU regardless of guest load and would dwarf real work).
// Used as a fallback when `/1.0/instances/<name>/state` reports cpu.usage=0
// — typical for VMs that don't have lxd-agent installed inside the guest,
// where the host-side cgroup never sees per-process CPU accounting.
// Returns 0 on any error so the caller can keep its existing value.
func lxdPromCPUFor(name string) int64 {
	body, err := fetchLXDMetricsBody()
	if err != nil {
		return 0
	}
	var busySec float64
	for _, s := range parsePromText(body) {
		if s.labels["name"] != name || s.metric != "lxd_cpu_seconds_total" {
			continue
		}
		if m := s.labels["mode"]; m == "idle" || m == "steal" {
			continue
		}
		busySec += s.value
	}
	return int64(busySec * 1e9)
}

// lxdPromMemoryFor scrapes the LXD Prometheus endpoint and returns
// (used, total) bytes for the named instance. used is derived from
// MemTotal − MemAvailable (the only formula that works for both
// containers and VMs without lxd-agent inside the guest).
// Returns (0, 0) on any error so the caller can keep its existing value.
func lxdPromMemoryFor(name string) (used, total int64) {
	body, err := fetchLXDMetricsBody()
	if err != nil {
		return 0, 0
	}
	var memActiveAnon, memAvail, memTotal float64
	for _, s := range parsePromText(body) {
		if s.labels["name"] != name {
			continue
		}
		switch s.metric {
		case "lxd_memory_Active_anon_bytes":
			if s.value > memActiveAnon {
				memActiveAnon = s.value
			}
		case "lxd_memory_MemAvailable_bytes":
			if s.value > memAvail {
				memAvail = s.value
			}
		case "lxd_memory_MemTotal_bytes":
			if s.value > memTotal {
				memTotal = s.value
			}
		}
	}
	u := memActiveAnon
	if memTotal > 0 && memAvail > 0 && memTotal > memAvail {
		if v := memTotal - memAvail; v > u {
			u = v
		}
	}
	return int64(u), int64(memTotal)
}

// pidUptimeSec returns how long the process with the given host PID has been running,
// by reading /proc/{pid}/stat and /proc/uptime. Returns 0 on any error.
func pidUptimeSec(pid int64) int64 {
	uptimeData, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	var sysUptimeSec float64
	fmt.Sscanf(strings.TrimSpace(string(uptimeData)), "%f", &sysUptimeSec)

	statData, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// /proc/pid/stat: pid (comm) state ppid ... starttime(22nd field)
	// The comm field may contain spaces and is wrapped in parens; skip past the last ')'.
	s := string(statData)
	lastParen := strings.LastIndex(s, ")")
	if lastParen < 0 {
		return 0
	}
	fields := strings.Fields(s[lastParen+1:])
	// After ')': state(0) ppid(1) pgrp(2) session(3) tty(4) tpgid(5) flags(6)
	// minflt(7) cminflt(8) majflt(9) cmajflt(10) utime(11) stime(12) cutime(13) cstime(14)
	// priority(15) nice(16) num_threads(17) itrealvalue(18) starttime(19)
	if len(fields) < 20 {
		return 0
	}
	var startTicks int64
	fmt.Sscanf(fields[19], "%d", &startTicks)
	if startTicks == 0 {
		return 0
	}
	const clkTck = 100 // USER_HZ, standard on Linux x86/arm
	processStartSec := float64(startTicks) / clkTck
	up := sysUptimeSec - processStartSec
	if up < 0 {
		return 0
	}
	return int64(up)
}

// USBDevice is a USB device found on the host.
type USBDevice struct {
	Bus       string `json:"bus"`
	Device    string `json:"device"`
	VendorID  string `json:"vendor_id"`
	ProductID string `json:"product_id"`
	Desc      string `json:"desc"`
}

// PCIDevice is a PCI device found on the host.
type PCIDevice struct {
	Slot   string `json:"slot"`
	Class  string `json:"class"`
	Vendor string `json:"vendor"`
	Device string `json:"device"`
}

var lxdNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9-]*$`)

// pciAddrRe accepts both short (BB:SS.F) and full (DDDD:BB:SS.F) PCI addresses.
var pciAddrRe = regexp.MustCompile(`^([0-9a-fA-F]{4}:)?[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-9a-fA-F]$`)

// normPCIAddr expands a short-form PCI address "BB:SS.F" to "0000:BB:SS.F".
func normPCIAddr(addr string) string {
	if strings.Count(addr, ":") == 1 {
		return "0000:" + addr
	}
	return addr
}

var usbIDRe = regexp.MustCompile(`^[0-9a-fA-F]{4}$`)

// parsePCIQEMUArgs extracts per-device options from a raw.qemu string.
// Returns a map keyed by *both* the normalised PCI host address AND the
// Incus device ID (`dev-incus_<devname>`) so the GET path can look up
// options no matter which emission form was used. Two recognised forms:
//
//   - New form (ZNAS ≥ 6.5.20): `-set device.dev-incus_<name>.<prop>=<val>`
//     — modifies the device that Incus' own qemu.conf already emits for
//     a `type: pci` device. Avoids the "device is already attached"
//     QEMU error that the legacy form triggered on hosts where Incus'
//     and ZNAS' vfio-pci frontends both tried to claim the same host
//     device.
//   - Legacy form (≤ 6.5.19): `-device vfio-pci,host=<addr>,<prop>=<val>,…`
//     — was a second `-device` line, kept here so an old raw.qemu still
//     decodes cleanly. New writes always use the -set form; the next
//     `applyPCIRawQEMU` pass cleans the legacy entry out.
func parsePCIQEMUArgs(rawQEMU string) map[string]map[string]string {
	result := map[string]map[string]string{}

	// Legacy: `-device vfio-pci,...`, keyed by host PCI address.
	devRe := regexp.MustCompile(`-device\s+vfio-pci,([^\s]+)`)
	for _, m := range devRe.FindAllStringSubmatch(rawQEMU, -1) {
		opts := map[string]string{}
		for _, kv := range strings.Split(m[1], ",") {
			if parts := strings.SplitN(kv, "=", 2); len(parts) == 2 {
				opts[parts[0]] = parts[1]
			}
		}
		if host := opts["host"]; host != "" {
			result[normPCIAddr(host)] = opts
		}
	}

	// New: `-set device.<id>.<prop>=<val>`, keyed by the device ID. Each
	// occurrence carries exactly one prop=val; we merge by id.
	setRe := regexp.MustCompile(`-set\s+device\.(dev-incus_[A-Za-z0-9_.-]+)\.([A-Za-z0-9_-]+)=([^\s]+)`)
	for _, m := range setRe.FindAllStringSubmatch(rawQEMU, -1) {
		id, prop, val := m[1], m[2], m[3]
		if result[id] == nil {
			result[id] = map[string]string{}
		}
		result[id][prop] = val
	}

	return result
}

// buildPCIQEMUArg returns the raw.qemu fragment that overrides per-device
// vfio-pci properties (rombar / x-vga / aer) for a given Incus PCI device,
// or "" when no overrides are requested.
//
// We MUST emit `-set device.<id>.<prop>=<val>` rather than a fresh
// `-device vfio-pci,host=<addr>,…`: Incus already emits a vfio-pci device
// for every `type: pci` entry (visible in `/run/incus/<vm>/qemu.conf` as
// `[device "dev-incus_<DeviceName>"]`), and adding a SECOND `-device`
// targeting the same host BDF makes QEMU fail VM start with
//
//   vfio <addr>: device is already attached
//
// The `-set` form modifies the already-defined device in place, so the
// host's vfio-pci binding is claimed exactly once. The Incus device ID
// is deterministic: `dev-incus_<DeviceName>` (verified against mediaserver
// on Ubuntu 26.04 + Incus 6.0.4, May 2026).
//
// QEMU's vfio-pci device tree also distinguishes numeric vs boolean
// properties:
//   - rombar is uint32 (accepts 0 or 1)
//   - x-vga and aer are bool — QEMU rejects "1"/"0" with
//     "Parameter 'x-vga' expects 'on' or 'off'"
//
// The frontend dropdown stores "1"/"0"/"" for all three for UI uniformity,
// so pciBoolToken normalises the boolean ones to QEMU's required form.
// pciManagedSetRe matches the exact `-set device.dev-incus_*.PROP=VAL`
// directives that ZNAS itself emits — only the five properties the UI
// exposes (rombar / x-vga / aer / x-igd-opregion / x-igd-gms). Anything
// else on `device.dev-incus_*` (x-no-mmap, vendor-id, device-id, …) is
// admin-owned and must survive an Edit-then-Save round-trip. Keeping the
// property allowlist explicit here means the strip step in applyPCIRawQEMU
// can never silently drop a manually-added option.
//
// x-igd-opregion and x-igd-gms were added to the UI in v6.5.26 after the
// mediaserver investigation showed they're both required for stable Intel
// iGPU passthrough (without them, sustained VAAPI transcoding wedges i915).
// Before that, admins had to add them via SSH and the strip regex preserved
// them via the orphan-strip exception.
var pciManagedSetRe = regexp.MustCompile(`\s*-set\s+device\.dev-incus_[A-Za-z0-9_.-]+\.(?:rombar|x-vga|aer|x-igd-opregion|x-igd-gms)=[^\s]+`)

// pciOrphanSetRe matches `-set device.<ID>.<prop>=<val>` and captures the
// full Incus device ID in group 1. applyPCIRawQEMU uses this to find
// references to PCI devices that no longer exist in the instance config
// and strip them — QEMU rejects a `-set` against an unknown device with
// `there is no device "X" defined`, blocking VM start. Captures only the
// device-ID segment; the property name and value are consumed but not
// captured.
var pciOrphanSetRe = regexp.MustCompile(`\s*-set\s+device\.(dev-incus_[A-Za-z0-9_.-]+)\.[A-Za-z0-9_-]+=[^\s]+`)

func buildPCIQEMUArg(pci LXDPCIDevice) string {
	if pci.ROMBar == "" && pci.XVGA == "" && pci.AER == "" && pci.XIGDOpRegion == "" && pci.XIGDGMS == "" {
		return ""
	}
	if pci.DeviceName == "" {
		// Without a DeviceName we can't build the Incus-side device ID, so
		// fall back to the legacy `-device vfio-pci` shape. The caller
		// guards against this for normal flows; this is just defensive.
		parts := []string{"host=" + normPCIAddr(pci.Address)}
		if pci.ROMBar != "" {
			parts = append(parts, "rombar="+pci.ROMBar)
		}
		if v := pciBoolToken(pci.XVGA); v != "" {
			parts = append(parts, "x-vga="+v)
		}
		if v := pciBoolToken(pci.AER); v != "" {
			parts = append(parts, "aer="+v)
		}
		if v := pciBoolToken(pci.XIGDOpRegion); v != "" {
			parts = append(parts, "x-igd-opregion="+v)
		}
		if pci.XIGDGMS != "" {
			parts = append(parts, "x-igd-gms="+pci.XIGDGMS)
		}
		return "-device vfio-pci," + strings.Join(parts, ",")
	}
	devID := "dev-incus_" + pci.DeviceName
	var sets []string
	if pci.ROMBar != "" {
		sets = append(sets, "-set device."+devID+".rombar="+pci.ROMBar)
	}
	if v := pciBoolToken(pci.XVGA); v != "" {
		sets = append(sets, "-set device."+devID+".x-vga="+v)
	}
	if v := pciBoolToken(pci.AER); v != "" {
		sets = append(sets, "-set device."+devID+".aer="+v)
	}
	// x-igd-opregion is a QEMU boolean (on/off). Required for Intel iGPU
	// passthrough — gives the guest's i915 driver access to the OpRegion
	// ACPI buffer (firmware/power/panel info). Without it, i915 wedges
	// under sustained transcoding load.
	if v := pciBoolToken(pci.XIGDOpRegion); v != "" {
		sets = append(sets, "-set device."+devID+".x-igd-opregion="+v)
	}
	// x-igd-gms is numeric (uint32, 0..16). Reserves N×32MB of stolen
	// memory the iGPU uses as VAAPI encoder DMA buffer. Plex/ffmpeg under
	// Coffee Lake needs at least 2 (64MB).
	if pci.XIGDGMS != "" {
		sets = append(sets, "-set device."+devID+".x-igd-gms="+pci.XIGDGMS)
	}
	return strings.Join(sets, " ")
}

// pciBoolToken normalizes any of "1"/"on"/"true"/"yes" → "on" and
// "0"/"off"/"false"/"no" → "off" for QEMU vfio-pci boolean properties.
// Empty input maps to "" so callers can skip emission. Anything else is
// returned unchanged so a hand-typed value (rare, advanced users) still
// reaches QEMU verbatim instead of getting silently dropped.
func pciBoolToken(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return ""
	case "1", "on", "true", "yes":
		return "on"
	case "0", "off", "false", "no":
		return "off"
	default:
		return v
	}
}

// pciBoolFromQEMU converts QEMU's on/off (or numeric 0/1) form for boolean
// vfio-pci properties back into the "1"/"0" the frontend's PCI-options
// dropdown expects. Inverse of pciBoolToken on the read path so a config
// round-trip (raw.qemu → /api/lxd/instances/<vm>/config → modal) keeps
// the dropdown highlighted on the correct entry.
func pciBoolFromQEMU(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "":
		return ""
	case "on", "true", "yes", "1":
		return "1"
	case "off", "false", "no", "0":
		return "0"
	default:
		return v
	}
}

// ── Disable Virtual VGA helpers (raw.qemu.conf override) ────────────────────
//
// Incus' default QEMU template always emits a `[device "qemu_gpu"]` virtio-vga
// in the generated /run/incus/<vm>/qemu.conf. That's fine for normal VMs but
// actively harmful for VMs doing physical-GPU passthrough — the guest kernel
// binds fbcon to the virtio-vga (or to the passed-through iGPU if x-vga=on),
// and on imperfect IGD passthrough the framebuffer init half-completes,
// leaving every TTY mutex permanently held → "task blocked for >122 s" /
// shutdown hangs (confirmed against a Plex VM on 192.168.2.5, May 2026).
//
// Incus exposes raw.qemu.conf as a *key-level override*: setting
//
//   [device "qemu_gpu"]
//   driver = "pcie-pci-bridge"
//
// replaces just the `driver` key on the existing qemu_gpu device, leaving
// the bus/addr untouched. pcie-pci-bridge is a passive PCIe-to-PCI bridge
// with no display function — the guest sees a benign empty bus at the slot
// the virtio-vga used to live at, fbcon never binds, the shutdown hang
// goes away. Verified live against vm-1.
//
// State tracking via comment markers IN the raw.qemu.conf was tried in
// 6.5.21 but rejected — QEMU's config-file parser refuses the merged file
// with "Expected section header, got: '# znas-disable-virtual-vga-start'"
// because Incus' merger placed the comment lines in positions where QEMU
// expects either a [section] or a key=value. Instead we track state with a
// dedicated instance user config key (disableVGAUserKey) and keep
// raw.qemu.conf strictly to QEMU-parseable content.
const disableVGAUserKey = "user.znas:disable_virtual_vga"

// disableVGAOverrideBody is the exact raw.qemu.conf payload ZNAS writes when
// the user enables "Disable virtual VGA adapter". One section, one key —
// no comments, no markers, no surrounding blank lines (they get added at
// concat time when raw.qemu.conf already has user content).
const disableVGAOverrideBody = `[device "qemu_gpu"]
driver = "pcie-pci-bridge"`

// disableVGAStripRe matches the exact section+driver line pair we emit so
// a toggle-off removes ZNAS' contribution without disturbing any unrelated
// raw.qemu.conf content the user may have added themselves. (?m) anchors
// `^` to line boundaries; `\s*` after each line tolerates trailing
// whitespace; the optional trailing newline soaks up a single blank line
// after the block.
var disableVGAStripRe = regexp.MustCompile(`(?m)^\[device "qemu_gpu"\][ \t]*\r?\ndriver[ \t]*=[ \t]*"pcie-pci-bridge"[ \t]*\r?\n?`)

// applyDisableVirtualVGA returns the updated raw.qemu.conf content for the
// given disable setting. Pure function — caller is responsible for the
// `incus config set/unset` calls and for keeping disableVGAUserKey in
// sync. Idempotent: re-applying with the same setting on already-current
// input is a no-op.
func applyDisableVirtualVGA(currentRawConf string, disable bool) string {
	stripped := disableVGAStripRe.ReplaceAllString(currentRawConf, "")
	stripped = strings.TrimRight(stripped, "\n")
	if !disable {
		return stripped
	}
	if stripped == "" {
		return disableVGAOverrideBody
	}
	return stripped + "\n\n" + disableVGAOverrideBody
}

// readDisableVirtualVGA reads the ZNAS-managed state flag for "Disable
// virtual VGA adapter" from the instance's user config. The user key is
// the single source of truth — raw.qemu.conf may have been edited by the
// admin out-of-band, and we don't try to second-guess that here.
func readDisableVirtualVGA(rawConfig map[string]string) bool {
	return rawConfig[disableVGAUserKey] == "true"
}

// applyPCIRawQEMU rewrites the instance raw.qemu config key to include
// per-device vfio-pci overrides. All existing -device vfio-pci entries are
// removed and replaced with entries derived from pciDevices (only those with
// at least one option set are written back). Other raw.qemu content is kept.
func applyPCIRawQEMU(name string, pciDevices []LXDPCIDevice) {
	out, _ := exec.Command("incus", "config", "get", name, "raw.qemu").Output()
	existing := strings.TrimSpace(string(out))

	// Strip all existing ZNAS-managed PCI override entries — both the
	// legacy `-device vfio-pci,...` shape (≤ 6.5.19, kept around so
	// upgrade-then-edit cleans them out) AND the `-set device.dev-incus_*`
	// shape we emit now.
	//
	// Two strip rules with different scopes:
	//
	//  1. For DEVICES THAT STILL EXIST in pciDevices: only strip ZNAS-managed
	//     props (rombar / x-vga / aer) so we can re-emit them from current UI
	//     state. Admin-added props on the same device (x-igd-opregion=on for
	//     Intel iGPU OpRegion mapping, x-no-mmap=on, vendor-id, …) pass
	//     through untouched. Stripping unknown props caused a 6.5.22
	//     regression where iGPU passthrough lost its OpRegion after any
	//     unrelated Edit save.
	//
	//  2. For DEVICES THAT NO LONGER EXIST in pciDevices: strip EVERY `-set
	//     device.dev-incus_<gone>.*` line, including admin-added props.
	//     QEMU rejects a `-set` directive that targets a non-existent device
	//     with `there is no device "dev-incus_X" defined` and the VM won't
	//     start. This was a 6.5.23 regression: removing the iGPU passthrough
	//     in the UI left an orphan `-set device.dev-incus_pci0.x-igd-opregion=on`
	//     behind, refusing to start the VM until raw.qemu was manually edited.
	devRe := regexp.MustCompile(`\s*-device\s+vfio-pci,[^\s]*`)
	existing = devRe.ReplaceAllString(existing, "")

	// Rule 1: strip managed props on any device (will be re-emitted from
	// pciDevices below if device still exists).
	existing = pciManagedSetRe.ReplaceAllString(existing, "")

	// Rule 2: strip every `-set device.dev-incus_<X>.*` whose <X> is no
	// longer in pciDevices. Compute the keep-set first so the regex
	// callback can answer in O(1).
	keep := map[string]bool{}
	for _, pci := range pciDevices {
		if pci.DeviceName != "" {
			keep["dev-incus_"+pci.DeviceName] = true
		}
	}
	existing = pciOrphanSetRe.ReplaceAllStringFunc(existing, func(m string) string {
		// Match groups: leading whitespace ... `-set device.<id>.<prop>=<val>`
		sub := pciOrphanSetRe.FindStringSubmatch(m)
		if len(sub) < 2 {
			return m
		}
		if keep[sub[1]] {
			return m // device still exists, preserve admin-added prop
		}
		return "" // orphan reference to a removed device — strip
	})
	existing = strings.TrimSpace(existing)

	// Build replacement entries for devices with extra options.
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
	newVal := strings.Join(parts, " ")

	if newVal == "" {
		exec.Command("incus", "config", "unset", name, "raw.qemu").Run()
	} else {
		// key=value single-arg form: prevents `incus`'s flag parser from
		// treating values that start with "-" (e.g. "-global …" or
		// "-smp sockets=2") as shorthand flags.
		exec.Command("incus", "config", "set", name, "raw.qemu="+newVal).Run()
	}
}

// LXDAvailable probes LXD accessibility by running `lxc list --format json`.
func LXDAvailable() bool {
	cmd := exec.Command("incus", "list", "--format", "json")
	return cmd.Run() == nil
}

// LXDVersion returns the lxc client version string.
func LXDVersion() string {
	out, err := exec.Command("incus", "version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// lxdStateNetwork is the per-interface state as returned by lxc list / lxc query state.
type lxdStateNetwork struct {
	State     string `json:"state"` // "up" | "down"
	HWAddr    string `json:"hwaddr"`
	Addresses []struct {
		Family  string `json:"family"`
		Address string `json:"address"`
		Scope   string `json:"scope"`
	} `json:"addresses"`
}

// lxdPickBestIP selects the most relevant global IPv4 for a running instance.
// It prefers IPs from interfaces that correspond to a configured LXD NIC device,
// using name-match first then MAC-match via volatile expanded_config entries.
// Only falls back to unmatched interfaces if no NIC device interface is found,
// filtering out loopback and known virtual/internal bridge prefixes.
func lxdPickBestIP(
	expandedDevices map[string]map[string]string,
	expandedConfig map[string]string,
	network map[string]lxdStateNetwork,
) string {
	// Build a set of NIC device names and their volatile MACs.
	type nicEntry struct{ name, mac string }
	var nics []nicEntry
	for dev, cfg := range expandedDevices {
		if cfg["type"] != "nic" {
			continue
		}
		mac := cfg["hwaddr"]
		if mac == "" && expandedConfig != nil {
			mac = expandedConfig["volatile."+dev+".hwaddr"]
		}
		nics = append(nics, nicEntry{dev, mac})
	}

	usedIPs := map[string]bool{}

	// Pass 1: name-match — works reliably for containers.
	for _, nic := range nics {
		if iface, ok := network[nic.name]; ok {
			for _, a := range iface.Addresses {
				if a.Family == "inet" && a.Scope == "global" {
					usedIPs[a.Address] = true
					return a.Address
				}
			}
		}
	}

	// Pass 2: MAC-match — needed for VMs where the OS renames interfaces.
	for _, nic := range nics {
		if nic.mac == "" {
			continue
		}
		for _, iface := range network {
			if strings.EqualFold(iface.HWAddr, nic.mac) {
				for _, a := range iface.Addresses {
					if a.Family == "inet" && a.Scope == "global" {
						usedIPs[a.Address] = true
						return a.Address
					}
				}
			}
		}
	}

	// Pass 3: fallback — any global IPv4 from a non-loopback, non-virtual interface.
	// Exclude known internal/virtual bridge prefixes to avoid picking the wrong IP.
	internalPrefixes := []string{"lo", "lxdbr", "docker", "virbr", "veth", "br-lxc"}
	for ifName, iface := range network {
		isInternal := false
		for _, pfx := range internalPrefixes {
			if strings.HasPrefix(ifName, pfx) {
				isInternal = true
				break
			}
		}
		if isInternal {
			continue
		}
		for _, a := range iface.Addresses {
			if a.Family == "inet" && a.Scope == "global" {
				return a.Address
			}
		}
	}
	return ""
}

// ListLXDInstances returns all LXD instances (VMs + containers).
func ListLXDInstances() ([]LXDInstance, error) {
	out, err := exec.Command("incus", "list", "--format", "json").Output()
	if err != nil {
		return nil, fmt.Errorf("lxc list: %w", err)
	}

	var raw []struct {
		Name            string                       `json:"name"`
		Description     string                       `json:"description"`
		Type            string                       `json:"type"`
		Status          string                       `json:"status"`
		Config          map[string]string            `json:"config"`
		ExpandedDevices map[string]map[string]string `json:"expanded_devices"`
		ExpandedConfig  map[string]string            `json:"expanded_config"`
		State           *struct {
			Network map[string]lxdStateNetwork `json:"network"`
		} `json:"state"`
	}
	// boot.autostart can be set either directly on the instance (raw config) or
	// inherited from a profile (expanded_config). The table column shows the
	// effective value, so we read expanded_config.
	autostartTrue := func(v string) bool { return v == "true" || v == "1" }
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("lxc list parse: %w", err)
	}

	instances := make([]LXDInstance, 0, len(raw))
	for _, r := range raw {
		inst := LXDInstance{
			Name:        r.Name,
			Description: r.Description,
			Type:        r.Type,
			Status:      r.Status,
			CPULimit:    r.Config["limits.cpu"],
			MemoryLimit: r.Config["limits.memory"],
			Autostart:   autostartTrue(r.ExpandedConfig["boot.autostart"]),
		}

		// Derive image description from config.
		if d := r.Config["image.description"]; d != "" {
			inst.Image = d
		} else {
			osName := r.Config["image.os"]
			ver := r.Config["image.version"]
			if osName != "" || ver != "" {
				inst.Image = strings.TrimSpace(osName + " " + ver)
			}
		}

		// Pick best IPv4: prefer IPs from NIC-device interfaces over internal bridges.
		if r.State != nil && r.State.Network != nil {
			inst.IPv4 = lxdPickBestIP(r.ExpandedDevices, r.ExpandedConfig, r.State.Network)
		}

		// Find the root disk's storage pool.
		for _, dev := range r.ExpandedDevices {
			if dev["type"] == "disk" && dev["path"] == "/" && dev["pool"] != "" {
				inst.RootPool = dev["pool"]
				break
			}
		}

		instances = append(instances, inst)
	}
	return instances, nil
}

// LXDGetStatus returns the current status string of a named instance.
func LXDGetStatus(name string) (string, error) {
	if !lxdNameRe.MatchString(name) {
		return "", fmt.Errorf("invalid instance name")
	}
	out, err := exec.Command("incus", "list", name, "--format", "json").Output()
	if err != nil {
		return "", err
	}
	var raw []struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(out, &raw); err != nil || len(raw) == 0 {
		return "", fmt.Errorf("not found")
	}
	return raw[0].Status, nil
}

// LXDStart starts a stopped instance.
func LXDStart(name string) error {
	if !lxdNameRe.MatchString(name) {
		return fmt.Errorf("invalid instance name")
	}
	out, err := exec.Command("incus", "start", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}

// LXDStop stops a running instance gracefully (force=false) or immediately (force=true).
func LXDStop(name string, force bool) error {
	if !lxdNameRe.MatchString(name) {
		return fmt.Errorf("invalid instance name")
	}
	// Tell the state watcher this is a portal-initiated stop so it doesn't
	// emit the "VM stopped unexpectedly" alert.
	MarkUserInitiatedStop(name)
	args := []string{"stop", name}
	if force {
		args = append(args, "--force")
	} else {
		// Use a short server-side timeout so the HTTP handler returns promptly.
		// The "shutdown or kill" flow on the client manages the real deadline and
		// sends a force stop when needed. Without this, lxc stop blocks the
		// goroutine for the VM's full ACPI shutdown wait (30 s+).
		args = append(args, "--timeout=10")
	}
	out, err := exec.Command("incus", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}

// LXDRestart restarts a running instance.
func LXDRestart(name string) error {
	if !lxdNameRe.MatchString(name) {
		return fmt.Errorf("invalid instance name")
	}
	MarkUserInitiatedStop(name)
	out, err := exec.Command("incus", "restart", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}

// LXDReset cold-boots an instance — equivalent to pressing a physical
// hardware Reset button. Translates to `incus restart --force`, which
// kills QEMU/the container without an ACPI shutdown and brings it back
// up. Used by the VGA console's keyboard menu so a guest stuck before
// it can respond to Ctrl+Alt+Del (e.g. boot loader, BSOD, OVMF setup)
// can still be recovered.
func LXDReset(name string) error {
	if !lxdNameRe.MatchString(name) {
		return fmt.Errorf("invalid instance name")
	}
	MarkUserInitiatedStop(name)
	out, err := exec.Command("incus", "restart", "--force", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}

// LXDDelete deletes an instance. Always uses --force to handle ERROR/running states.
func LXDDelete(name string, deleteVolumes bool) error {
	if !lxdNameRe.MatchString(name) {
		return fmt.Errorf("invalid instance name")
	}
	// Stop the instance first (ignore errors — may already be stopped or in ERROR state).
	_ = LXDStop(name, true)
	// Always pass --force so deletion succeeds even when the instance is in ERROR state
	// or when lxc considers it still running after a failed stop.
	return exec.Command("incus", "delete", "--force", name).Run()
}

// LXDNICConfig describes a network interface device on an instance.
type LXDNICConfig struct {
	Name        string `json:"name"`
	Bridge      string `json:"bridge"`    // "network" or "parent" value
	NICType     string `json:"nictype"`   // "network" (managed) or "bridged" (direct bridge)
	Connected   bool   `json:"connected"` // false when OS link is down (detected from instance state)
	VlanID      int    `json:"vlan_id,omitempty"`
	FromProfile bool   `json:"from_profile,omitempty"`
	MAC         string `json:"mac,omitempty"`        // volatile.<name>.hwaddr
	CurrentIP   string `json:"current_ip,omitempty"` // live IPv4 from instance state
	IPv4Mode    string `json:"ipv4_mode,omitempty"`  // "dhcp" | "static" | "none"
	IPv4Addr    string `json:"ipv4_addr,omitempty"`  // e.g. "10.0.0.10/24"
	IPv4GW      string `json:"ipv4_gw,omitempty"`    // gateway IP
	DNS1        string `json:"dns1,omitempty"`       // primary DNS (static only)
	DNS2        string `json:"dns2,omitempty"`       // secondary DNS (static only)
}

// LXDDiskConfig describes a disk device on an instance.
type LXDDiskConfig struct {
	Name         string `json:"name"`
	Pool         string `json:"pool,omitempty"`
	Size         string `json:"size,omitempty"`
	ReservePct   int    `json:"reserve_pct,omitempty"` // 0=thin, 25/50/75/100
	IsRoot       bool   `json:"is_root,omitempty"`
	IsAgent      bool   `json:"is_agent,omitempty"`    // true when source=="agent:config" — synthetic Incus agent share
	FromProfile  bool   `json:"from_profile,omitempty"`
	ZFSPath      string `json:"zfs_path,omitempty"`      // backing ZFS path
	ZFSType      string `json:"zfs_type,omitempty"`      // "zvol" | "dataset"
	CompRatio    string `json:"comp_ratio,omitempty"`    // e.g. "1.23x"
	BootPriority string `json:"boot_priority,omitempty"` // lxd boot.priority value
	// VM-only per-disk knobs. Empty string means "leave unset / inherit".
	IOCache  string `json:"io_cache,omitempty"` // "" | "none" | "writeback" | "writethrough" | "unsafe" | "directsync"
	IOBus    string `json:"io_bus,omitempty"`   // "" | "virtio-blk" | "virtio-scsi" | "nvme" — overrides the VM-wide DiskBus when non-empty
	ReadOnly bool   `json:"readonly,omitempty"` // attach disk read-only
}

// LXDCapabilities flags optional disk knobs by LXD's API extension list, so
// the UI can render only the keys the running LXD will actually accept
// (LXD 5.0.x lacks `io.bus`/`io.threads`, breaks the dropdown silently).
type LXDCapabilities struct {
	DiskIOBus     bool `json:"disk_io_bus"`
	DiskIOCache   bool `json:"disk_io_cache"`
	DiskIOThreads bool `json:"disk_io_threads"`
}

// LXDInstanceConfig holds the editable configuration of an LXD instance.
type LXDInstanceConfig struct {
	Description       string `json:"description"`
	CPULimit          string `json:"cpu_limit"`
	CPUPin            string `json:"cpu_pin"`     // LXD range string for pinning; overrides CPULimit when non-empty
	CPUSockets        int    `json:"cpu_sockets"` // QEMU socket topology (0=auto)
	MemoryLimit       string `json:"memory_limit"`
	MemoryHugepages   bool   `json:"memory_hugepages"`
	MemoryReservation string `json:"memory_reservation"` // "", "25", "50", "75", "100", or "custom:<size>"
	Nesting           bool   `json:"nesting"`
	Autostart         bool   `json:"autostart"`
	// ForceRunning controls whether ZNAS auto-restarts the instance after an
	// unexpected stop (guest poweroff, QEMU crash, OOM, external CLI stop).
	// Stored as the Incus user.* key `user.zfsnas.force_running`, separate
	// from Autostart so the two flags can be toggled independently.
	ForceRunning      bool   `json:"force_running"`
	StatefulSnapshots bool   `json:"stateful_snapshots"` // migration.stateful — VM-only
	IsVM              bool   `json:"is_vm"`
	// Container-specific features (only applied when ApplyContainerFeatures is true)
	ApplyContainerFeatures bool                   `json:"apply_container_features,omitempty"`
	CPULimitPct            int                    `json:"cpu_limit_pct,omitempty"`  // 0=unset, 1-100 → limits.cpu.allowance
	CPUShares              int                    `json:"cpu_shares,omitempty"`     // 0=unset, 1-10 → limits.cpu.priority
	SwapLimit              string                 `json:"swap_limit,omitempty"`     // "" | "false" | "512MB"
	// Tri-state on the wire would be ideal, but we keep the bool and
	// drop `omitempty` so the field is ALWAYS present in the GET
	// response. With omitempty a privileged container (Unprivileged
	// false = the bool's zero value) silently dropped the field, and
	// the frontend's `cfg.unprivileged !== false` default flipped the
	// checkbox back to "Unprivileged" on the next edit, masking the
	// successful save.
	Unprivileged           bool                   `json:"unprivileged"`             // security.privileged = !Unprivileged
	FeatureKeyctl          bool                   `json:"feature_keyctl,omitempty"` // security.syscalls.allow=keyctl
	FeatureFUSE            bool                   `json:"feature_fuse,omitempty"`   // /dev/fuse device
	CDROMPath              string                 `json:"cdrom_path"`               // current ISO path (GET) / desired path (PUT)
	ApplyCDROM             bool                   `json:"apply_cdrom"`              // if true, apply CDROMPath change on PUT (legacy single-drive)
	CDROMs                 []string               `json:"cdroms"`                   // handler-resolved absolute ISO paths (multi-drive)
	ApplyCDROMs            bool                   `json:"apply_cdroms"`             // if true, replace all CDROMs with CDROMs list
	Firmware               string                 `json:"firmware"`                 // "uefi" (default) | "bios"
	SecureBoot             bool                   `json:"secure_boot"`              // only meaningful when Firmware == "uefi"
	TPM                    bool                   `json:"tpm"`                      // enable emulated TPM 2.0 (security.tpm)
	MachineType            string                 `json:"machine_type"`             // "" = auto, "pc-q35-9.1", "pc-i440fx-9.1", etc.
	DiskBus                string                 `json:"disk_bus"`                 // "" = virtio-blk (default), "scsi", "nvme"
	SMBIOS                 *LXDSMBIOSType1        `json:"smbios,omitempty"`         // SMBIOS type 1 (System Information); applied via raw.qemu
	SMBIOSType2            *LXDSMBIOSType2        `json:"smbios_type2,omitempty"`   // SMBIOS type 2 (Baseboard / Motherboard); applied via raw.qemu
	SMBIOSType4            *LXDSMBIOSType4        `json:"smbios_type4,omitempty"`   // SMBIOS type 4 (Processor); applied via raw.qemu
	DisableVirtualVGA      bool                   `json:"disable_virtual_vga"`      // replace Incus' default virtio-vga with a passive bridge; used for full GPU passthrough so the guest doesn't bind a framebuffer console
	NICs                   []LXDNICConfig         `json:"nics"`
	Disks                  []LXDDiskConfig        `json:"disks"`
	DetachDisks            []string               `json:"detach_disks,omitempty"` // device names to detach only (keep backing volume)
	ExistingDisks          []LXDExistingDisk      `json:"existing_disks_raw"`     // ZVols to attach as new raw block devices
	USBDevices             []LXDUSBDevice         `json:"usb_devices"`
	PCIDevices             []LXDPCIDevice         `json:"pci_devices"`
	PassthroughDevices     []LXDPassthroughDevice `json:"passthrough_devices"`
	// Daemon-side capability flags (read-only on GET; ignored on PUT). Lets
	// the editor disable inputs that the running LXD will silently reject.
	Capabilities LXDCapabilities `json:"capabilities,omitempty"`
}

var lxdDevNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// updateRawQEMUSockets inserts or updates a -smp sockets=N flag in a raw.qemu string.
func updateRawQEMUSockets(rawQemu string, sockets int) string {
	// Remove any existing -smp sockets= clause we added.
	const marker = " -smp sockets="
	if idx := strings.Index(rawQemu, marker); idx >= 0 {
		end := idx + len(marker)
		for end < len(rawQemu) && rawQemu[end] >= '0' && rawQemu[end] <= '9' {
			end++
		}
		rawQemu = strings.TrimSpace(rawQemu[:idx]) + rawQemu[end:]
	}
	if sockets > 1 {
		rawQemu = strings.TrimSpace(rawQemu) + fmt.Sprintf(" -smp sockets=%d", sockets)
	}
	return strings.TrimSpace(rawQemu)
}

// smbiosAddString appends a key=value pair to parts, encoded so QEMU's
// option parser AND Incus' raw.qemu argv tokenizer both see what we intend.
//
// Two distinct hazards:
//
//  1. Commas inside the value collide with QEMU's `,`-separated key=value
//     pairs. QEMU's documented escape is a doubled comma, so we apply that.
//
//  2. Whitespace inside the value collides with Incus' raw.qemu tokenizer
//     (it splits the string on whitespace before exec'ing QEMU, so a value
//     with a space arrives as two argv elements and QEMU rejects the
//     second one). There is no escape for this — Incus owns the argv
//     boundary. We replace internal whitespace with `_` as a best-effort
//     so the imported guest at least boots. The previous `.base64=on`
//     trick was wrong: that companion option doesn't exist in QEMU
//     (verified against QEMU 10.0.8 — `Invalid parameter 'manufacturer.base64'`),
//     so values were unconditionally rejected at boot.
//
// Empty values are skipped so QEMU falls back to firmware/board defaults.
func smbiosAddString(parts *[]string, key, val string) {
	val = strings.TrimSpace(val)
	if val == "" {
		return
	}
	val = strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' {
			return '_'
		}
		return r
	}, val)
	val = strings.ReplaceAll(val, ",", ",,")
	*parts = append(*parts, key+"="+val)
}

// smbiosAddInt appends an integer key=value pair. Zero is treated as "unset"
// and omitted, matching the JSON omitempty contract on the struct fields.
func smbiosAddInt(parts *[]string, key string, val int) {
	if val == 0 {
		return
	}
	*parts = append(*parts, fmt.Sprintf("%s=%d", key, val))
}

// stripExistingSMBIOSClause removes any "-smbios <typePrefix>,…" or
// "-smbios <typePrefix>" clause from a raw.qemu string while preserving
// other -smbios clauses (different types) and unrelated raw.qemu content.
//
// Token-based: smbiosAddString guarantees no whitespace inside the value,
// so we can rely on strings.Fields and treat each `-smbios <next-token>`
// pair as a clause. The previous regex-style implementation used a marker
// of " -smbios " (with a leading space), which silently failed to match a
// clause sitting at position 0 — every Proxmox-imported VM ended up with
// the type=1 clause duplicated on the next save.
func stripExistingSMBIOSClause(rawQemu, typePrefix string) string {
	fields := strings.Fields(rawQemu)
	out := make([]string, 0, len(fields))
	for i := 0; i < len(fields); i++ {
		if fields[i] == "-smbios" && i+1 < len(fields) {
			v := fields[i+1]
			if strings.HasPrefix(v, typePrefix+",") || v == typePrefix {
				i++ // skip the value too
				continue
			}
		}
		out = append(out, fields[i])
	}
	return strings.Join(out, " ")
}

// parseSMBIOSClause locates the first "-smbios <typePrefix>,…" clause in
// rawQemu and returns its decoded key→value map. ok=false when no matching
// clause exists, so callers can return nil rather than an empty struct.
//
// Round-trips two encodings written by smbiosAddString:
//   - QEMU's doubled-comma escape (",," → literal ',').
//   - Underscore-for-space substitution stays as-is (lossy on the read
//     path; we have no way to know which underscores were originally
//     spaces). That's fine — the UI will just show the underscore form.
//
// Also still decodes the legacy "<key>.base64=on" companion option for
// clauses written by versions ≤ 6.5.4 (which QEMU rejected at boot, but
// users may still have those values cached in raw.qemu until they re-save).
func parseSMBIOSClause(rawQemu, typePrefix string) (map[string]string, bool) {
	fields := strings.Fields(rawQemu)
	for i := 0; i < len(fields); i++ {
		if fields[i] != "-smbios" || i+1 >= len(fields) {
			continue
		}
		val := fields[i+1]
		if !strings.HasPrefix(val, typePrefix+",") && val != typePrefix {
			continue
		}
		base64Set := map[string]bool{}
		raw := map[string]string{}
		for _, kv := range splitEscapedCommas(val) {
			eq := strings.IndexByte(kv, '=')
			if eq < 0 {
				continue
			}
			k, v := kv[:eq], kv[eq+1:]
			if strings.HasSuffix(k, ".base64") && v == "on" {
				base64Set[strings.TrimSuffix(k, ".base64")] = true
				continue
			}
			raw[k] = v
		}
		for k, v := range raw {
			if base64Set[k] {
				if b, err := base64.StdEncoding.DecodeString(v); err == nil {
					raw[k] = string(b)
				}
			}
		}
		return raw, true
	}
	return nil, false
}

// splitEscapedCommas splits on `,` but treats `,,` as a literal comma,
// matching QEMU's option-parser escape rule (the inverse of what
// smbiosAddString writes).
func splitEscapedCommas(s string) []string {
	var out []string
	var cur strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			if i+1 < len(s) && s[i+1] == ',' {
				cur.WriteByte(',')
				i++
				continue
			}
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(s[i])
	}
	out = append(out, cur.String())
	return out
}

// updateRawQEMUSMBIOSType1 inserts or updates a "-smbios type=1,…" clause in
// a raw.qemu string. Removes any prior type=1 clause we wrote so the values
// stay in sync with the form. A nil/empty SMBIOS struct strips the clause
// entirely.
func updateRawQEMUSMBIOSType1(rawQemu string, s *LXDSMBIOSType1) string {
	rawQemu = stripExistingSMBIOSClause(rawQemu, "type=1")
	if s == nil {
		return strings.TrimSpace(rawQemu)
	}
	parts := []string{"type=1"}
	smbiosAddString(&parts, "uuid", s.UUID)
	smbiosAddString(&parts, "manufacturer", s.Manufacturer)
	smbiosAddString(&parts, "product", s.Product)
	smbiosAddString(&parts, "version", s.Version)
	smbiosAddString(&parts, "serial", s.Serial)
	smbiosAddString(&parts, "sku", s.SKU)
	smbiosAddString(&parts, "family", s.Family)
	if len(parts) == 1 {
		return strings.TrimSpace(rawQemu)
	}
	return strings.TrimSpace(strings.TrimSpace(rawQemu) + " -smbios " + strings.Join(parts, ","))
}

// parseRawQEMUSMBIOSType1 reads a raw.qemu string and decodes the type=1
// clause back into an LXDSMBIOSType1. Returns nil when no clause is present
// so the GET handler can omit the field entirely from JSON output.
func parseRawQEMUSMBIOSType1(rawQemu string) *LXDSMBIOSType1 {
	raw, ok := parseSMBIOSClause(rawQemu, "type=1")
	if !ok {
		return nil
	}
	out := &LXDSMBIOSType1{
		UUID:         raw["uuid"],
		Manufacturer: raw["manufacturer"],
		Product:      raw["product"],
		Version:      raw["version"],
		Serial:       raw["serial"],
		SKU:          raw["sku"],
		Family:       raw["family"],
	}
	if *out == (LXDSMBIOSType1{}) {
		return nil
	}
	return out
}

// updateRawQEMUSMBIOSType2 inserts or updates a "-smbios type=2,…" clause
// (Baseboard / Motherboard Information). Mirrors the type 1 helper.
func updateRawQEMUSMBIOSType2(rawQemu string, s *LXDSMBIOSType2) string {
	rawQemu = stripExistingSMBIOSClause(rawQemu, "type=2")
	if s == nil {
		return strings.TrimSpace(rawQemu)
	}
	parts := []string{"type=2"}
	smbiosAddString(&parts, "manufacturer", s.Manufacturer)
	smbiosAddString(&parts, "product", s.Product)
	smbiosAddString(&parts, "version", s.Version)
	smbiosAddString(&parts, "serial", s.Serial)
	smbiosAddString(&parts, "asset", s.Asset)
	smbiosAddString(&parts, "location", s.Location)
	if len(parts) == 1 {
		return strings.TrimSpace(rawQemu)
	}
	return strings.TrimSpace(strings.TrimSpace(rawQemu) + " -smbios " + strings.Join(parts, ","))
}

// parseRawQEMUSMBIOSType2 decodes the type=2 clause back into a struct.
// Returns nil when absent or all-empty so JSON omits the field.
func parseRawQEMUSMBIOSType2(rawQemu string) *LXDSMBIOSType2 {
	raw, ok := parseSMBIOSClause(rawQemu, "type=2")
	if !ok {
		return nil
	}
	out := &LXDSMBIOSType2{
		Manufacturer: raw["manufacturer"],
		Product:      raw["product"],
		Version:      raw["version"],
		Serial:       raw["serial"],
		Asset:        raw["asset"],
		Location:     raw["location"],
	}
	if *out == (LXDSMBIOSType2{}) {
		return nil
	}
	return out
}

// updateRawQEMUSMBIOSType4 inserts or updates a "-smbios type=4,…" clause
// (Processor Information). String fields use the shared base64-aware
// encoder; integer fields (max-speed, current-speed, processor-family) are
// emitted plain — QEMU parses them as %d.
func updateRawQEMUSMBIOSType4(rawQemu string, s *LXDSMBIOSType4) string {
	rawQemu = stripExistingSMBIOSClause(rawQemu, "type=4")
	if s == nil {
		return strings.TrimSpace(rawQemu)
	}
	parts := []string{"type=4"}
	smbiosAddString(&parts, "sock_pfx", s.SockPfx)
	smbiosAddString(&parts, "manufacturer", s.Manufacturer)
	smbiosAddString(&parts, "version", s.Version)
	smbiosAddString(&parts, "serial", s.Serial)
	smbiosAddString(&parts, "asset", s.Asset)
	smbiosAddString(&parts, "part", s.Part)
	smbiosAddInt(&parts, "processor-family", s.ProcessorFamily)
	smbiosAddInt(&parts, "max-speed", s.MaxSpeed)
	smbiosAddInt(&parts, "current-speed", s.CurrentSpeed)
	if len(parts) == 1 {
		return strings.TrimSpace(rawQemu)
	}
	return strings.TrimSpace(strings.TrimSpace(rawQemu) + " -smbios " + strings.Join(parts, ","))
}

// parseRawQEMUSMBIOSType4 decodes the type=4 clause back into a struct.
// Returns nil when absent or all-empty so JSON omits the field.
func parseRawQEMUSMBIOSType4(rawQemu string) *LXDSMBIOSType4 {
	raw, ok := parseSMBIOSClause(rawQemu, "type=4")
	if !ok {
		return nil
	}
	atoi := func(s string) int {
		n, _ := strconv.Atoi(strings.TrimSpace(s))
		return n
	}
	out := &LXDSMBIOSType4{
		SockPfx:         raw["sock_pfx"],
		Manufacturer:    raw["manufacturer"],
		Version:         raw["version"],
		Serial:          raw["serial"],
		Asset:           raw["asset"],
		Part:            raw["part"],
		ProcessorFamily: atoi(raw["processor-family"]),
		MaxSpeed:        atoi(raw["max-speed"]),
		CurrentSpeed:    atoi(raw["current-speed"]),
	}
	if *out == (LXDSMBIOSType4{}) {
		return nil
	}
	return out
}

// machineFlagRe matches a `-machine TYPE` argv pair anywhere in a raw.qemu
// string — including at the very start, where the leading-space lookup in
// the old implementation missed and silently accumulated duplicates
// (`-machine q35 -machine q35 …`). The non-capturing leading `(?:^|\s)` lets
// the strip work at position 0 too. Followed by a single non-space token so
// only the TYPE argument is consumed, not the next flag.
var machineFlagRe = regexp.MustCompile(`(?:^|\s)-machine\s+\S+`)

// updateRawQEMUMachine inserts or updates a -machine TYPE flag in a raw.qemu
// string. Used because Incus 6.0.x rejects the qemu.machine.type config key
// ("Unknown configuration key") — only the qemu_raw_conf extension is
// available, so we override the machine type via raw.qemu instead. machineType
// of "" removes ALL previously-injected -machine clauses.
func updateRawQEMUMachine(rawQemu, machineType string) string {
	// Strip every existing -machine clause. This handles three real-world
	// shapes: (1) one clause mid-string, (2) one clause at the very start
	// of raw.qemu where the previous strings.Index(" -machine ") lookup
	// missed it, and (3) accumulated duplicates from past saves that hit
	// case 2 and appended without cleaning up.
	rawQemu = machineFlagRe.ReplaceAllString(rawQemu, "")
	rawQemu = strings.TrimSpace(rawQemu)
	machineType = strings.TrimSpace(machineType)
	if machineType == "" {
		return rawQemu
	}
	if rawQemu == "" {
		return "-machine " + machineType
	}
	return rawQemu + " -machine " + machineType
}

// cdromDriveRe matches -drive ...media=cdrom... entries in a raw.qemu string.
// QEMU drive values are comma-separated with no internal spaces (file paths with spaces are
// not valid in LXD ISO names), so \S+ captures the entire value without crossing into other flags.
var cdromDriveRe = regexp.MustCompile(`\s*-drive\s+\S*media=cdrom\S*`)

// cdromIdeDevRe matches -device ide-cd,... entries (the SATA/ICH9 AHCI half
// of a CDROM declaration on Q35). Paired with cdromDriveRe so we strip both
// halves cleanly when reconciling.
var cdromIdeDevRe = regexp.MustCompile(`\s*-device\s+ide-cd\S*`)

// cdromFileRe extracts the file= path from a -drive media=cdrom entry.
var cdromFileRe = regexp.MustCompile(`-drive\s+file=([^,\s]+)[^-]*media=cdrom`)

// cdromAAre matches AppArmor rules added by ZNAS for ISO directories.
// Covers both old-style /.isos/ rules and new-style snap isos rules.
var cdromAAre = regexp.MustCompile(`\S+/(?:\.isos|isos/[^/]+)/\*\* rk,`)

// bootFlagRe matches a `-boot OPTS` argv pair in a raw.qemu string. Used by
// setBootStrictOff to find and rewrite an existing -boot clause so we can
// flip strict=on (Incus' default on Debian 13 / Ubuntu 26.04) to strict=off
// without leaving a stray duplicate behind.
var bootFlagRe = regexp.MustCompile(`\s*-boot\s+\S+`)

// cdromBootindexBase is the bootindex floor for raw.qemu CDROMs. Incus
// auto-assigns bootindex 0 to the root disk and 1 to eth0; using 10+i keeps
// us clear of those slots while still leaving the CDROMs in OVMF's boot
// list (after the root + PXE attempts time out, the firmware falls through
// to the CDROM and boots the installer).
//
// The fall-through requires `-boot strict=off`. Incus passes
// `-boot strict=on` by default on Debian 13 / Ubuntu 26.04, which made OVMF
// halt at the first non-bootable device (the empty root zvol at bootindex=0)
// instead of iterating to the CDROM — the firmware printed
// "BdsDxe: failed to load Boot0001 […] not found" and then "No OS to boot
// from" without ever touching the SATA CDROM. setBootStrictOff overrides
// this whenever a CDROM is attached.
const cdromBootindexBase = 10

// cdromBootPriorityBase is the boot.priority floor used when adding a CDROM
// as an Incus disk-device (the canonical `boot.priority=10` install path).
// Higher Incus priority = lower QEMU bootindex, so cdrom0 lands at bootindex
// 0 — strictly ahead of the root disk and eth0 — and OVMF boots straight
// from the disc. Subsequent CDROMs descend (cdrom1=9, cdrom2=8, …) so the
// first ISO is always tried first.
const cdromBootPriorityBase = 10

// setBootStrictOff ensures the raw.qemu string contains
// `-boot order=dcn,strict=off` so OVMF can:
//
//  1. Walk through every BootOrder entry until one boots, instead of halting
//     at the first failed entry (`strict=off`). Without this, fresh VMs with
//     an empty root disk + PXE-less NIC + a CDROM never reach the CDROM and
//     report "no OS to boot from" — even though the CDROM is attached and
//     bootable.
//
//  2. Try CD-ROM first, then disk, then network for any device that lacks an
//     explicit bootindex (`order=dcn`). bootindex hints on individual
//     devices override `order=` per QEMU semantics, so this is harmless when
//     Incus has set bootindex on the root disk and NIC; it kicks in as a
//     fallback when Incus does not.
//
// `menu=on` was tried briefly in v6.5.10 to expose the F12 boot picker
// automatically. It was dropped in v6.5.11 after a production incident on
// 192.168.2.216: when no boot device works, OVMF with `menu=on` sits on
// its built-in boot manager screen and stops servicing QMP, which hangs
// Incus' periodic state queries, which hangs `incus list`, which hangs
// `system.LXDAvailable()` at zfsnas startup, which prevents the HTTPS
// listener from ever binding. The portal went dark. The F12 boot picker
// is still reachable from the VGA console toolbar (sendVGAKey(0x58)),
// so we don't need menu=on to expose it to the user.
//
// `reboot-timeout=N` is intentionally absent too: it is SeaBIOS-only
// (per QEMU docs) and OVMF ignores it; some QEMU builds warn about it
// on UEFI, which clutters the VM start log.
//
// Subsequent -boot flags override earlier ones in QEMU's option parser, so
// it's safe to append even when Incus has already emitted `-boot strict=on`.
// We still strip any prior -boot clause we may have added in earlier saves
// to keep the raw.qemu string from growing on every edit.
func setBootStrictOff(rawQemu string) string {
	rawQemu = bootFlagRe.ReplaceAllString(rawQemu, "")
	rawQemu = strings.TrimSpace(rawQemu)
	suffix := "-boot order=dcn,strict=off"
	if rawQemu == "" {
		return suffix
	}
	return rawQemu + " " + suffix
}

// setCDROMsRawQEMU replaces any existing cdrom -drive / -device ide-cd
// entries in rawQemu with entries for the given ISO paths. CDROMs are
// attached as SATA devices on the Q35 ICH9 AHCI controller (bus=ide.N),
// which Windows installs natively without needing virtio drivers loaded
// up-front. Each pair carries an explicit bootindex starting at
// cdromBootindexBase to avoid colliding with Incus' auto-assigned slots.
func setCDROMsRawQEMU(rawQemu string, paths []string) string {
	rawQemu = cdromDriveRe.ReplaceAllString(rawQemu, "")
	rawQemu = cdromIdeDevRe.ReplaceAllString(rawQemu, "")
	rawQemu = strings.TrimSpace(rawQemu)
	for i, p := range paths {
		if p == "" || !filepath.IsAbs(p) {
			continue
		}
		rawQemu = strings.TrimSpace(rawQemu) +
			fmt.Sprintf(" -drive file=%s,if=none,id=cd%d,media=cdrom,readonly=on -device ide-cd,drive=cd%d,bus=ide.%d,bootindex=%d",
				p, i, i, i, cdromBootindexBase+i)
	}
	return strings.TrimSpace(rawQemu)
}

// setCDROMsAppArmor replaces ISO directory AppArmor rules in rawAA with the
// appropriate rules for the given ISO paths.
//
// Incus on Debian is deb-only — no snap mount-namespace tricks are needed.
// We emit one rule per unique ISO parent directory; QEMU's AppArmor profile
// extends from those.
func setCDROMsAppArmor(rawAA string, paths []string) string {
	rawAA = strings.TrimSpace(cdromAAre.ReplaceAllString(rawAA, ""))
	{
		seen := map[string]bool{}
		for _, p := range paths {
			if p == "" || !filepath.IsAbs(p) {
				continue
			}
			dir := filepath.Dir(p)
			if !seen[dir] {
				seen[dir] = true
				rawAA = strings.TrimSpace(rawAA) + " " + dir + "/** rk,"
			}
		}
	}
	return strings.TrimSpace(rawAA)
}

// vmApplyCDROMs reconciles the CDROM set on a VM via the canonical Incus
// install path: each ISO is exposed as an Incus disk-device with
// `boot.priority=10`, so OVMF lands on the CDROM at QEMU bootindex 0 and
// boots straight into the installer's GRUB without iterating through the
// empty root disk or eth0's PXE/HTTP timeouts.
//
// Q35 SATA (raw.qemu `-device ide-cd,bus=ide.0`) is also emitted, but as a
// **fallback** for Windows installers — virtio-scsi requires a viostor
// side-load that fresh Windows ISOs don't carry, so OVMF skips Incus'
// default-bus CDROM almost instantly on those and falls through to the SATA
// copy. For Linux/BSD/macOS installs the Incus disk-device path wins first
// and the SATA copy is never reached. Both attachments point at the same
// ISO file; the guest just sees two CDROM devices with identical media.
//
// `-boot order=dcn,strict=off` is still injected so any auto-discovered
// devices (no explicit bootindex) prefer CD-ROM, and so OVMF iterates
// through every BootOrder entry instead of halting at the first failed
// one. **Never** `menu=on` — when no device boots, OVMF parks on its
// boot-manager screen and stops servicing QMP, which hangs Incus and
// transitively prevents zfsnas from binding 8443 at startup. Verified the
// hard way on production (192.168.2.216, May 2026 — see v6.5.10 incident
// notes).
//
// Constraint: Incus rejects file-source disk-devices on VMs with
// `migration.stateful=true` ("Only Incus-managed disks are allowed with
// migration.stateful=true"). For those VMs the dual-attach is impossible;
// we fall back to SATA-only via raw.qemu and the user lives with the slow
// PXE/HTTP fall-through. The fresh-VM-create path in `LXDCreateVM` defers
// `migration.stateful=true` whenever a CDROM is attached for exactly this
// reason (see the long comment around the migration.stateful set call).
//
// Migration: any cdrom* disk device with a stale source path is removed
// first so existing VMs get the current set on the next save.
func vmApplyCDROMs(name string, paths []string, applyConf func(string, string) error) {
	// 1. Drop every currently-attached readonly ISO disk device. We rebuild
	// from `paths` below.
	out, _ := exec.Command("incus", "query", "/1.0/instances/"+name).Output()
	var inst struct {
		Devices map[string]map[string]string `json:"devices"`
	}
	if json.Unmarshal(out, &inst) == nil {
		for devName, d := range inst.Devices {
			if d["type"] == "disk" && d["readonly"] == "true" &&
				strings.HasSuffix(strings.ToLower(d["source"]), ".iso") {
				exec.Command("incus", "config", "device", "remove", name, devName).Run() //nolint:errcheck
			}
		}
	}

	// 2. If `migration.stateful=true` is set on this VM, Incus refuses
	// file-source disk devices outright, so we can't dual-attach. Skip the
	// Incus-native step and fall through to raw.qemu SATA-only.
	statefulOut, _ := exec.Command("incus", "config", "get", name, "migration.stateful").Output()
	stateful := strings.TrimSpace(string(statefulOut)) == "true"

	if !stateful {
		// 3. Add an Incus disk-device per requested ISO, with descending
		// boot.priority so the first CDROM lands at bootindex 0, the second
		// at bootindex 1, etc. This is the canonical `incus config device
		// add … boot.priority=10` install pattern.
		for i, p := range paths {
			if p == "" || !filepath.IsAbs(p) {
				continue
			}
			devName := fmt.Sprintf("cdrom%d", i)
			if out, err := exec.Command("incus", "config", "device", "add", name, devName, "disk", //nolint:errcheck
				"source="+p, "readonly=true",
				fmt.Sprintf("boot.priority=%d", cdromBootPriorityBase-i),
			).CombinedOutput(); err != nil {
				// Surface the error so a future Incus regression doesn't
				// silently fail like v6.5.10 did. Caller sees this in the
				// activity log; we still emit raw.qemu SATA below as a
				// fallback so the user isn't left with no CDROM at all.
				_ = applyConf("user.znas:cdrom_attach_warning",
					fmt.Sprintf("incus config device add %s: %s",
						devName, strings.TrimSpace(string(out))))
			}
		}
	}

	// 4. Always emit the raw.qemu SATA copy: Windows installers need it
	// (no viostor), and on stateful VMs it's the only attachment.
	curRQ, _ := exec.Command("incus", "config", "get", name, "raw.qemu").Output()
	newRQ := setCDROMsRawQEMU(strings.TrimSpace(string(curRQ)), paths)
	hasPaths := false
	for _, p := range paths {
		if p != "" && filepath.IsAbs(p) {
			hasPaths = true
			break
		}
	}
	if hasPaths {
		newRQ = setBootStrictOff(newRQ)
	}
	if strings.TrimSpace(newRQ) != strings.TrimSpace(string(curRQ)) {
		lxdPatchConfig(name, "raw.qemu", newRQ)
	}

	// 5. Reconcile raw.apparmor: QEMU's sandbox needs explicit read access
	// to each ISO directory because the raw.qemu drive bypasses Incus'
	// layered AppArmor profile (the Incus-native disk-device path inherits
	// it automatically).
	curAA, _ := exec.Command("incus", "config", "get", name, "raw.apparmor").Output()
	newAA := setCDROMsAppArmor(strings.TrimSpace(string(curAA)), paths)
	if strings.TrimSpace(newAA) != strings.TrimSpace(string(curAA)) {
		applyConf("raw.apparmor", newAA) //nolint:errcheck
	}
}

// lxdSupportsConfigKey probes whether the running LXD version accepts a given
// instance config key. Result is cached per process. Used to gate optional
// keys like security.syscalls.intercept.keyctl that exist on newer LXD but
// not on LXD 5.0.x. Implementation: try `lxc config set` on a throwaway
// project-default profile is too invasive — instead we ask LXD's own metadata
// endpoint, which lists every supported key.
var (
	lxdSupportedKeysOnce sync.Once
	lxdSupportedKeys     map[string]bool
)

// LXDGetCapabilities returns flags for the optional disk knobs we surface in
// the Edit modal. Values are derived from the LXD daemon's api_extensions
// list — LXD 5.0.2 (Debian stable) ships without `disk_io_bus`, so the
// io.bus dropdown silently fails for users; reading the extensions lets the
// UI grey out keys that won't apply.
//
// Cached for the process lifetime: api_extensions only change with an LXD
// upgrade and our process restarts on update.
func LXDGetCapabilities() LXDCapabilities {
	lxdAPIExtensionsOnce.Do(func() {
		lxdAPIExtensions = map[string]bool{}
		out, err := exec.Command("incus", "query", "/1.0").Output()
		if err != nil {
			return
		}
		var resp struct {
			APIExtensions []string `json:"api_extensions"`
		}
		if err := json.Unmarshal(out, &resp); err != nil {
			return
		}
		for _, e := range resp.APIExtensions {
			lxdAPIExtensions[e] = true
		}
	})
	return LXDCapabilities{
		DiskIOBus:     lxdAPIExtensions["disk_io_bus"],
		DiskIOCache:   lxdAPIExtensions["disk_io_cache"],
		DiskIOThreads: lxdAPIExtensions["disk_io_threads"],
	}
}

var (
	lxdAPIExtensionsOnce sync.Once
	lxdAPIExtensions     map[string]bool
)

func lxdSupportsConfigKey(key string) bool {
	lxdSupportedKeysOnce.Do(func() {
		lxdSupportedKeys = map[string]bool{}
		out, err := exec.Command("incus", "query", "/1.0/metadata/configuration").Output()
		if err != nil {
			return
		}
		// Walk the metadata recursively looking for any field literally
		// named the config key. The metadata schema is nested and varies
		// across LXD versions, so a string scan is the most resilient.
		// We scan for the key embedded in JSON — false positives are
		// fine because we'll fall back to LXD's own validation if a
		// caller still tries to set it.
		s := string(out)
		// Pre-extract every "key" name; LXD's metadata uses `"<keyname>": {...}`
		// pattern at the leaves.
		for _, line := range strings.Split(s, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, `"`) {
				continue
			}
			end := strings.Index(line[1:], `"`)
			if end < 0 {
				continue
			}
			name := line[1 : 1+end]
			if strings.Contains(name, ".") {
				lxdSupportedKeys[name] = true
			}
		}
	})
	return lxdSupportedKeys[key]
}

// normalizeSwapLimit normalizes a user-supplied limits.memory.swap value to
// what LXD actually accepts: a boolean string ("true"/"false"), or empty to
// leave the key unset. Sizes (e.g. "2GB", "2048MB") are silently coerced to
// "true" (swap allowed) because LXD has no per-instance swap-size limit —
// passing the size as-is fails with "Invalid value for a boolean".
func normalizeSwapLimit(v string) string {
	t := strings.TrimSpace(strings.ToLower(v))
	switch t {
	case "", "true", "false", "0", "1":
		return t
	}
	// Anything else (size strings, bytes literal, etc.) → "true".
	// "false"-like sentinels handled above; everything that survives means
	// "user wants swap to exist", which in LXD terms is just "true".
	return "true"
}

// lxdPatchConfig sets or unsets a single LXD config key using the REST API,
// avoiding lxc config set's flag-parsing issues with values that start with '-'.
// lxdPatchConfig sets a single config key on an instance without going through
// `lxc query -X PATCH /1.0/instances/<name>`. We avoid that PATCH endpoint
// because LXD treats it as a full-resource patch: any top-level field omitted
// from the body (notably `description`) is silently reset to its zero value.
// That bit us when LXDSetConfig sets the description first and then later
// touches raw.qemu — the second call would wipe the description we just set.
//
// Instead, use `lxc config set name key=value` (the equals form), which lets
// us pass values that begin with "-" (e.g. raw.qemu="-smp …") without lxc
// interpreting them as CLI flags, and only updates the one config key.
func lxdPatchConfig(name, key, val string) {
	if val == "" {
		exec.Command("incus", "config", "unset", name, key).Run() //nolint:errcheck
		return
	}
	exec.Command("incus", "config", "set", name, key+"="+val).Run() //nolint:errcheck
}

// lxdFindZFSVol searches for a ZFS volume whose name ends with /suffix or _suffix.
// The _suffix form covers LXD project-prefixed volumes (e.g. "default_vm-3-data1").
// Returns the full ZFS dataset path or "".
func lxdFindZFSVol(suffix string) string {
	out, err := exec.Command("zfs", "list", "-H", "-t", "volume", "-o", "name").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasSuffix(line, "/"+suffix) || strings.HasSuffix(line, "_"+suffix) {
			return line
		}
	}
	return ""
}

// lxdSetZvolReservation sets refreservation on a ZFS zvol to pct% of its actual volsize.
// pct==0 clears the reservation (thin provisioning).
// Always queries ZFS for the real volsize to avoid string-parsing precision loss.
func lxdSetZvolReservation(zfsPath string, pct int) {
	if zfsPath == "" {
		return
	}
	if pct <= 0 {
		exec.Command("sudo", "zfs", "set", "refreservation=none", zfsPath).Run()
		return
	}
	out, err := exec.Command("zfs", "get", "-Hp", "volsize", zfsPath).Output()
	if err != nil {
		return
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) < 3 {
		return
	}
	volsize, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || volsize <= 0 {
		return
	}
	reserve := volsize * int64(pct) / 100
	exec.Command("sudo", "zfs", "set", fmt.Sprintf("refreservation=%d", reserve), zfsPath).Run()
}

// lxdZFSPoolForLXDPool returns the ZFS pool name backing a given LXD storage pool.
// Returns "" on error or if the pool driver is not ZFS.
func lxdZFSPoolForLXDPool(lxdPool string) string {
	out, err := exec.Command("incus", "query", "/1.0/storage-pools/"+lxdPool).Output()
	if err != nil {
		return ""
	}
	var sp struct {
		Driver string            `json:"driver"`
		Config map[string]string `json:"config"`
	}
	if err := json.Unmarshal(out, &sp); err != nil || sp.Driver != "zfs" {
		return ""
	}
	if v := sp.Config["zfs.pool_name"]; v != "" {
		return v
	}
	return sp.Config["source"]
}

// zfsGetUsedForPaths returns a map of zfs path → "used" bytes for the
// supplied paths in a single `zfs get` fork. Used by LXDGetInstanceStats
// to aggregate disk usage across every disk on an instance — Incus's
// own state JSON only reports the root disk.
func zfsGetUsedForPaths(paths []string) map[string]int64 {
	out := map[string]int64{}
	if len(paths) == 0 {
		return out
	}
	args := append([]string{"get", "-Hp", "-o", "name,value", "used"}, paths...)
	data, err := exec.Command("zfs", args...).Output()
	if err != nil {
		return out
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if v, perr := strconv.ParseInt(fields[1], 10, 64); perr == nil {
			out[fields[0]] = v
		}
	}
	return out
}

// zfsGetCompRatio returns the compressratio property for a ZFS dataset/zvol.
// Returns "" on error or if the value is "1.00x" (no compression benefit).
func zfsGetCompRatio(path string) string {
	out, err := exec.Command("zfs", "get", "-H", "-o", "value", "compressratio", path).Output()
	if err != nil {
		return ""
	}
	v := strings.TrimSpace(string(out))
	if v == "" || v == "-" || v == "1.00x" {
		return ""
	}
	return v
}

// LXDGetConfig fetches the editable config and devices for a named instance.
// Uses a single lxc query call; expanded_devices includes profile-inherited devices.
func LXDGetConfig(name string) (LXDInstanceConfig, error) {
	if !lxdNameRe.MatchString(name) {
		return LXDInstanceConfig{}, fmt.Errorf("invalid instance name")
	}
	out, err := exec.Command("incus", "query", "/1.0/instances/"+name).Output()
	if err != nil {
		return LXDInstanceConfig{}, fmt.Errorf("query instance: %w", err)
	}
	var raw struct {
		Description     string                       `json:"description"`
		Type            string                       `json:"type"` // "virtual-machine" | "container"
		Config          map[string]string            `json:"config"`
		ExpandedConfig  map[string]string            `json:"expanded_config"`
		Devices         map[string]map[string]string `json:"devices"`
		ExpandedDevices map[string]map[string]string `json:"expanded_devices"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return LXDInstanceConfig{}, err
	}
	if raw.Config == nil {
		raw.Config = map[string]string{}
	}
	// Parse socket count from raw.qemu if present.
	rawQemuSockets := 0
	if rq := raw.Config["raw.qemu"]; strings.Contains(rq, "-smp sockets=") {
		const marker = "-smp sockets="
		idx := strings.Index(rq, marker) + len(marker)
		end := idx
		for end < len(rq) && rq[end] >= '0' && rq[end] <= '9' {
			end++
		}
		if v, err := strconv.Atoi(rq[idx:end]); err == nil {
			rawQemuSockets = v
		}
	}
	// Parse machine type — prefer the native qemu.machine.type key (newer
	// Incus), fall back to the -machine TYPE token in raw.qemu (Incus 6.0.x
	// where the native key is rejected as "Unknown configuration key").
	machineType := raw.Config["qemu.machine.type"]
	if machineType == "" {
		if rq := raw.Config["raw.qemu"]; strings.Contains(rq, "-machine ") {
			const marker = "-machine "
			idx := strings.Index(rq, marker) + len(marker)
			end := idx
			for end < len(rq) && rq[end] != ' ' {
				end++
			}
			machineType = rq[idx:end]
		}
	}

	cpuLimitPct := 0
	fmt.Sscanf(raw.Config["limits.cpu.allowance"], "%dms/100ms", &cpuLimitPct)
	cpuShares := 0
	fmt.Sscanf(raw.Config["limits.cpu.priority"], "%d", &cpuShares)

	firmware := "uefi"
	if raw.Config["security.csm"] == "true" {
		firmware = "bios"
	}
	// Use expanded_config for secureboot: it reflects the effective value after
	// profile inheritance. Instance-level config overrides are visible here too.
	// Secure Boot is on by default for UEFI unless explicitly set to "false".
	effectiveCfg := raw.ExpandedConfig
	if effectiveCfg == nil {
		effectiveCfg = raw.Config
	}
	secureBoot := firmware == "uefi" && effectiveCfg["security.secureboot"] != "false"
	// LXD 6.x removed security.secureboot; fall back to detecting the raw.qemu
	// pflash override flag we inject as a workaround for that LXD version.
	if secureBoot && strings.Contains(raw.Config["raw.qemu"], lxdSBOffMarker) {
		secureBoot = false
	}
	cfg := LXDInstanceConfig{
		Description:       raw.Description,
		CPULimit:          raw.Config["limits.cpu"],
		CPUSockets:        rawQemuSockets,
		MemoryLimit:       raw.Config["limits.memory"],
		MemoryHugepages:   raw.Config["limits.memory.hugepages"] == "true",
		MemoryReservation: raw.Config["user.memory_reservation"],
		Nesting:           raw.Config["security.nesting"] == "true",
		Autostart:         raw.Config["boot.autostart"] == "true" || raw.Config["boot.autostart"] == "1",
		ForceRunning:      raw.Config["user.zfsnas.force_running"] == "true" || raw.Config["user.zfsnas.force_running"] == "1",
		StatefulSnapshots: raw.Config["migration.stateful"] == "true",
		IsVM:              raw.Type == "virtual-machine",
		Firmware:          firmware,
		SecureBoot:        secureBoot,
		MachineType:       machineType,
		// SMBIOS types 1/2/4 are round-tripped through raw.qemu's -smbios
		// clauses. nil when no clause is present so the JSON omits the field.
		SMBIOS:      parseRawQEMUSMBIOSType1(raw.Config["raw.qemu"]),
		SMBIOSType2: parseRawQEMUSMBIOSType2(raw.Config["raw.qemu"]),
		SMBIOSType4: parseRawQEMUSMBIOSType4(raw.Config["raw.qemu"]),
		// "Disable virtual VGA adapter" — state is tracked via the
		// user.znas:disable_virtual_vga config key (NOT by parsing
		// raw.qemu.conf — see disableVGAOverrideBody comments for why).
		DisableVirtualVGA: readDisableVirtualVGA(raw.Config),
		// Container-specific (populated for containers, ignored for VMs)
		CPULimitPct:  cpuLimitPct,
		CPUShares:    cpuShares,
		SwapLimit:    raw.Config["limits.memory.swap"],
		Unprivileged: raw.Config["security.privileged"] != "true",
		FeatureKeyctl: raw.Config["security.syscalls.intercept.keyctl"] == "true" ||
			strings.Contains(raw.Config["security.syscalls.allow"], "keyctl"),
		Capabilities: LXDGetCapabilities(),
	}
	for devName, devCfg := range raw.ExpandedDevices {
		_, isInstanceLevel := raw.Devices[devName]
		switch devCfg["type"] {
		case "tpm":
			cfg.TPM = true
		case "usb":
			cfg.USBDevices = append(cfg.USBDevices, LXDUSBDevice{
				DeviceName: devName,
				VendorID:   devCfg["vendorid"],
				ProductID:  devCfg["productid"],
			})
		case "pci":
			cfg.PCIDevices = append(cfg.PCIDevices, LXDPCIDevice{
				DeviceName: devName,
				Address:    devCfg["address"],
			})
		case "nic":
			nicType := devCfg["nictype"]
			bridge := devCfg["network"]
			if bridge == "" {
				bridge = devCfg["parent"]
			}
			if nicType == "" {
				if devCfg["network"] != "" {
					nicType = "network"
				} else {
					nicType = "bridged"
				}
			}
			vlanID := 0
			if v := devCfg["vlan"]; v != "" {
				fmt.Sscanf(v, "%d", &vlanID)
			}
			// Prefer device-level hwaddr, fall back to volatile expanded_config.
			mac := devCfg["hwaddr"]
			if mac == "" {
				mac = raw.ExpandedConfig["volatile."+devName+".hwaddr"]
			}
			cfg.NICs = append(cfg.NICs, LXDNICConfig{
				Name:        devName,
				Bridge:      bridge,
				NICType:     nicType,
				Connected:   true, // always true; OS link-down is reflected in Pass 1 below
				VlanID:      vlanID,
				FromProfile: !isInstanceLevel,
				MAC:         mac,
			})
		case "disk":
			// Separate CD-ROM devices from regular disks.
			if devCfg["readonly"] == "true" && strings.HasSuffix(strings.ToLower(devCfg["source"]), ".iso") {
				cfg.CDROMs = append(cfg.CDROMs, devCfg["source"])
				if cfg.CDROMPath == "" {
					cfg.CDROMPath = devCfg["source"] // legacy field — first drive
				}
				continue
			}
			lxdPool := devCfg["pool"]
			isRoot := devCfg["path"] == "/"
			// "agent" is the synthetic Incus agent disk that lives in
			// every VM. Its source is the literal "agent:config", not a
			// pool/volume — Incus generates it on the fly each start.
			// Flagged so the editor can render it read-only and prevent
			// accidental detachment (Incus would re-add it anyway, but
			// the user shouldn't be allowed to think it's editable).
			isAgent := devCfg["source"] == "agent:config"
			disk := LXDDiskConfig{
				Name:         devName,
				Pool:         lxdPool,
				Size:         lxdNormalizeSizeStr(devCfg["size"]),
				IsRoot:       isRoot,
				IsAgent:      isAgent,
				FromProfile:  !isInstanceLevel,
				BootPriority: devCfg["boot.priority"],
				IOCache:      devCfg["io.cache"],
				IOBus:        devCfg["io.bus"],
				ReadOnly:     devCfg["readonly"] == "true",
			}
			// Capture the disk bus from the root disk to represent the VM-wide setting.
			if isRoot && cfg.DiskBus == "" && devCfg["io.bus"] != "" {
				cfg.DiskBus = devCfg["io.bus"]
			}
			// Resolve the backing ZFS path for this specific disk.
			if lxdPool != "" {
				if zfsPool := lxdZFSPoolForLXDPool(lxdPool); zfsPool != "" {
					var zfsPath, zfsType string
					if raw.Type == "virtual-machine" {
						if isRoot {
							zfsPath = zfsPool + "/virtual-machines/" + name + ".block"
							zfsType = "zvol"
						} else {
							volName := devCfg["source"]
							if volName == "" {
								volName = name + "-" + devName
							}
							// Use suffix search to handle LXD project prefixes (e.g. "default_volname").
							zfsPath = lxdFindZFSVol(volName)
							zfsType = "zvol"
						}
					} else {
						if isRoot {
							zfsPath = zfsPool + "/containers/" + name
							zfsType = "dataset"
						} else {
							volName := devCfg["source"]
							if volName == "" {
								volName = name + "-" + devName
							}
							zfsPath = lxdFindZFSVol(volName)
							zfsType = "zvol"
						}
					}
					disk.ZFSPath = zfsPath
					disk.ZFSType = zfsType
					disk.CompRatio = zfsGetCompRatio(zfsPath)
				}
			}
			// Read refreservation from ZFS for all zvol disks (root and non-root).
			// Also read volsize for non-root volumes since LXD omits size from device config.
			if disk.ZFSPath != "" && disk.ZFSType == "zvol" {
				if out, err := exec.Command("zfs", "get", "-Hp", "volsize,refreservation", disk.ZFSPath).Output(); err == nil {
					var volsize, refreserv int64
					for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
						fields := strings.Fields(line)
						if len(fields) < 3 {
							continue
						}
						switch fields[1] {
						case "volsize":
							volsize, _ = strconv.ParseInt(fields[2], 10, 64)
						case "refreservation":
							refreserv, _ = strconv.ParseInt(fields[2], 10, 64)
						}
					}
					if disk.Size == "" && volsize > 0 {
						// Round to nearest GB so the display is accurate.
						disk.Size = fmt.Sprintf("%dGB", (volsize+500000000)/1000000000)
					}
					if volsize > 0 && refreserv > 0 {
						pct := int(refreserv * 100 / volsize)
						// Snap to nearest standard tier using midpoints.
						switch {
						case pct >= 88:
							disk.ReservePct = 100
						case pct >= 63:
							disk.ReservePct = 75
						case pct >= 38:
							disk.ReservePct = 50
						case pct >= 13:
							disk.ReservePct = 25
						}
					}
				}
			}
			cfg.Disks = append(cfg.Disks, disk)
		default:
			// For containers: capture any other device type as generic passthrough.
			// The special "fuse" device is tracked via FeatureFUSE instead.
			if raw.Type != "virtual-machine" && isInstanceLevel {
				if devName == "fuse" && devCfg["type"] == "unix-char" && devCfg["path"] == "/dev/fuse" {
					cfg.FeatureFUSE = true
					continue
				}
				extra := map[string]string{}
				for k, v := range devCfg {
					if k != "type" && k != "path" {
						extra[k] = v
					}
				}
				cfg.PassthroughDevices = append(cfg.PassthroughDevices, LXDPassthroughDevice{
					DeviceName: devName,
					Type:       devCfg["type"],
					HostPath:   devCfg["path"],
					Extra:      extra,
				})
			}
		}
	}

	// Enrich NICs with live IP addresses from instance state.
	stateOut, err := exec.Command("incus", "query", "/1.0/instances/"+name+"/state").Output()
	if err == nil {
		var state struct {
			Network map[string]lxdStateNetwork `json:"network"`
		}
		if json.Unmarshal(stateOut, &state) == nil && state.Network != nil {
			// Pass 1: exact device-name match (works reliably for containers).
			// Also syncs Connected to the actual OS link state so that a NIC brought
			// down via ip-link-set is correctly reflected when the edit modal re-reads.
			for i, nic := range cfg.NICs {
				if iface, ok := state.Network[nic.Name]; ok {
					if iface.State == "down" {
						cfg.NICs[i].Connected = false
					}
					for _, addr := range iface.Addresses {
						if addr.Family == "inet" && addr.Scope == "global" {
							cfg.NICs[i].CurrentIP = addr.Address
							break
						}
					}
				}
			}
			// Pass 2: MAC-based match using volatile expanded_config entries.
			// Required for VMs where the OS renames the NIC (e.g. eth0 → enp5s0).
			for i, nic := range cfg.NICs {
				if cfg.NICs[i].CurrentIP != "" {
					continue
				}
				devMAC := raw.ExpandedConfig["volatile."+nic.Name+".hwaddr"]
				if devMAC == "" {
					continue
				}
				for _, iface := range state.Network {
					if strings.EqualFold(iface.HWAddr, devMAC) {
						for _, addr := range iface.Addresses {
							if addr.Family == "inet" && addr.Scope == "global" {
								cfg.NICs[i].CurrentIP = addr.Address
								break
							}
						}
						break
					}
				}
			}
		}
	}

	// Also read CDROMs from raw.qemu (new SATA/ICH9 style, added by vmApplyCDROMs).
	// This supplements any cdrom disk devices already found in expanded_devices above.
	if rawQEMU := raw.Config["raw.qemu"]; rawQEMU != "" {
		for _, m := range cdromFileRe.FindAllStringSubmatch(rawQEMU, -1) {
			path := m[1]
			if path == "" {
				continue
			}
			// Deduplicate: don't add if already present from expanded_devices.
			found := false
			for _, existing := range cfg.CDROMs {
				if existing == path {
					found = true
					break
				}
			}
			if !found {
				cfg.CDROMs = append(cfg.CDROMs, path)
				if cfg.CDROMPath == "" {
					cfg.CDROMPath = path
				}
			}
		}
	}

	// Enrich PCI devices with rombar/x-vga/aer from raw.qemu (LXD stores these
	// outside the device config via -device vfio-pci QEMU overrides).
	if rawQEMU := raw.ExpandedConfig["raw.qemu"]; rawQEMU != "" && len(cfg.PCIDevices) > 0 {
		qemuOpts := parsePCIQEMUArgs(rawQEMU)
		for i, pci := range cfg.PCIDevices {
			// Try the new key first (`-set device.dev-incus_<name>` form)
			// then fall back to the legacy address key. parsePCIQEMUArgs
			// populates both into the same map.
			opts, ok := qemuOpts["dev-incus_"+pci.DeviceName]
			if !ok {
				opts, ok = qemuOpts[normPCIAddr(pci.Address)]
			}
			if !ok {
				continue
			}
			// rombar is a numeric in QEMU and in the dropdown — pass through.
			cfg.PCIDevices[i].ROMBar = opts["rombar"]
			// x-vga and aer are QEMU booleans (on/off). Translate back
			// to the 1/0 form the frontend dropdown stores so a fresh
			// Edit Modal opens with the correct option highlighted.
			cfg.PCIDevices[i].XVGA = pciBoolFromQEMU(opts["x-vga"])
			cfg.PCIDevices[i].AER = pciBoolFromQEMU(opts["aer"])
			// x-igd-opregion is a boolean (on/off); same round-trip.
			cfg.PCIDevices[i].XIGDOpRegion = pciBoolFromQEMU(opts["x-igd-opregion"])
			// x-igd-gms is a numeric 0..16 — pass through as-is.
			cfg.PCIDevices[i].XIGDGMS = opts["x-igd-gms"]
		}
	}

	// Sort disks: root first (scsi0/sda), then alphabetically by device name.
	sort.SliceStable(cfg.Disks, func(i, j int) bool {
		if cfg.Disks[i].IsRoot != cfg.Disks[j].IsRoot {
			return cfg.Disks[i].IsRoot
		}
		return cfg.Disks[i].Name < cfg.Disks[j].Name
	})

	// Append disconnected NICs saved as user.disconnected_nics.<name> metadata.
	// These are NICs that were removed from LXD devices but should still appear
	// in the UI as disconnected so the user can reconnect them.
	knownNICNames := map[string]struct{}{}
	for _, n := range cfg.NICs {
		knownNICNames[n.Name] = struct{}{}
	}
	for k, v := range raw.ExpandedConfig {
		if !strings.HasPrefix(k, "user.disconnected_nics.") {
			continue
		}
		devName := strings.TrimPrefix(k, "user.disconnected_nics.")
		if _, already := knownNICNames[devName]; already {
			continue // device was re-added; metadata will be cleared on next save
		}
		var meta struct {
			Bridge string `json:"bridge"`
			MAC    string `json:"mac"`
			Vlan   string `json:"vlan"`
		}
		if err := json.Unmarshal([]byte(v), &meta); err != nil {
			continue
		}
		vlanID := 0
		fmt.Sscanf(meta.Vlan, "%d", &vlanID)
		cfg.NICs = append(cfg.NICs, LXDNICConfig{
			Name:      devName,
			Bridge:    meta.Bridge,
			NICType:   "bridged",
			Connected: false,
			VlanID:    vlanID,
			MAC:       meta.MAC,
		})
	}

	// Sort NICs alphabetically by device name (eth0, eth1, net0, …).
	sort.SliceStable(cfg.NICs, func(i, j int) bool {
		return cfg.NICs[i].Name < cfg.NICs[j].Name
	})

	return cfg, nil
}

// lxdSBOffMarker is the raw.qemu fragment used to disable secure boot on LXD 6.x
// (which removed the security.secureboot config key for VMs).
// It tells QEMU's cfi.pflash01 device (OVMF) not to require secure/SMM mode.
const lxdSBOffMarker = "driver=cfi.pflash01,property=secure,value=off"

// lxdSetSecureBootRawQEMU adds/removes the secure-boot-disable pflash flag from
// raw.qemu without clobbering any other flags already present.
func lxdSetSecureBootRawQEMU(name string, enable bool) error {
	out, _ := exec.Command("incus", "config", "get", name, "raw.qemu").Output()
	current := strings.TrimSpace(string(out))

	// Strip any existing pflash secure flag (handles both "-global <marker>" forms).
	cleaned := strings.ReplaceAll(current, "-global "+lxdSBOffMarker, "")
	cleaned = strings.TrimSpace(cleaned)

	if !enable {
		flag := "-global " + lxdSBOffMarker
		if cleaned == "" {
			cleaned = flag
		} else {
			cleaned = cleaned + " " + flag
		}
	}

	var setOut []byte
	var setErr error
	if cleaned == "" {
		if current == "" {
			return nil // raw.qemu was already absent and nothing to add — no-op
		}
		setOut, setErr = exec.Command("incus", "config", "unset", name, "raw.qemu").CombinedOutput()
	} else {
		// Use key=value form so lxc doesn't parse the leading "-global" as its own flag.
		setOut, setErr = exec.Command("incus", "config", "set", name, "raw.qemu="+cleaned).CombinedOutput()
	}
	if setErr != nil {
		msg := strings.TrimSpace(string(setOut))
		if strings.Contains(msg, "not currently set") {
			return nil // already absent — idempotent
		}
		if msg == "" {
			msg = setErr.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// LXDSetConfig applies editable config and device changes to a named instance.
// cfg.NICs/Disks represent the desired instance-level device state; the backend diffs
// against current instance devices (not profile devices) to compute add/update/remove ops.
func LXDSetConfig(name string, cfg LXDInstanceConfig) error {
	if !lxdNameRe.MatchString(name) {
		return fmt.Errorf("invalid instance name")
	}

	// "Disable virtual VGA adapter" — BIOS guard. SeaBIOS only writes
	// boot output to VGA (no native serial console), and Intel iGPUs
	// ship no standalone option ROM (their VBIOS lives in the host
	// firmware, accessed via OpRegion at runtime). So a BIOS guest with
	// no virtual VGA hangs forever inside SeaBIOS during display init —
	// confirmed empirically on Z370 + UHD 630 + QEMU 10.x + Incus 6.0.5:
	// QEMU runs cleanly, one vCPU spins at 85% in the VBIOS-execution
	// loop, the guest never reaches the bootloader, agent never starts.
	// Even the full Q35 IGD recipe (-machine q35,igd-passthru=on, iGPU
	// at pcie.0:02.0, x-vga + x-igd-opregion + x-igd-gms) fails without
	// an extracted-from-host VBIOS romfile, which is system-specific and
	// fragile. UEFI (OVMF) handles Intel iGPU init natively via OpRegion
	// so the option is safe there — Plex transcoding works either way
	// because the iGPU is still exclusively passed through to i915
	// regardless of whether the virtual VGA is present. Fail fast before
	// any partial config is written (descriptor PATCH, key updates).
	if cfg.IsVM && cfg.DisableVirtualVGA && cfg.Firmware == "bios" {
		return fmt.Errorf("disable_virtual_vga is not supported on BIOS (CSM) VMs: SeaBIOS needs a virtual VGA for boot output and Intel iGPUs have no standalone VBIOS option ROM. Either keep the virtual VGA on (the iGPU is still passed through and Plex still gets exclusive access via i915) or switch this VM to UEFI firmware")
	}

	// Resolve IsVM from LXD if the caller did not supply it.
	if !cfg.IsVM {
		if out, err := exec.Command("incus", "query", "/1.0/instances/"+name).Output(); err == nil {
			var inst struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(out, &inst) == nil && inst.Type == "virtual-machine" {
				cfg.IsVM = true
			}
		}
	}

	// Capability flags drive whether per-disk io.bus / io.cache writes are
	// even attempted; on LXD 5.0.x these keys are absent and would error.
	caps := LXDGetCapabilities()

	// Description via REST PATCH.
	descJSON, _ := json.Marshal(cfg.Description)
	if out, err := exec.Command("incus", "query", "-X", "PATCH",
		"/1.0/instances/"+name, "--data", fmt.Sprintf(`{"description":%s}`, descJSON)).CombinedOutput(); err != nil {
		return fmt.Errorf("description: %s", strings.TrimSpace(string(out)))
	}

	// CPU / memory / autostart via lxc config set.
	applyConf := func(key, val string) error {
		var out []byte
		var err error
		if val == "" {
			out, err = exec.Command("incus", "config", "unset", name, key).CombinedOutput()
			if err != nil && strings.Contains(string(out), "not currently set") {
				return nil
			}
		} else {
			// Use key=value form when value starts with "-" to prevent lxc from
			// interpreting it as its own CLI flag (e.g. raw.qemu=-global ...).
			if strings.HasPrefix(val, "-") {
				out, err = exec.Command("incus", "config", "set", name, key+"="+val).CombinedOutput()
			} else {
				out, err = exec.Command("incus", "config", "set", name, key, val).CombinedOutput()
			}
		}
		if err != nil {
			return fmt.Errorf("%s: %s", key, strings.TrimSpace(string(out)))
		}
		return nil
	}
	// CPU pinning (range string) takes precedence over bare count.
	// LXD's limits.cpu uses two different syntaxes selected by presence of
	// "," or "-": with separators it's a pin set (specific CPUs); without
	// separators it's a count (number of vCPUs). A user typing a single CPU
	// index like "5" would otherwise be interpreted as "5 vCPUs", not
	// "pinned to CPU 5". Normalize a bare positive integer in CPUPin to
	// "N-N" so the user's intent is preserved.
	effectiveCPU := cfg.CPULimit
	if cfg.CPUPin != "" {
		effectiveCPU = normalizeCPUPin(cfg.CPUPin)
	}
	if err := applyConf("limits.cpu", effectiveCPU); err != nil {
		return err
	}
	// CPU socket topology (VM-only).  Stored as raw.qemu override.
	if cfg.IsVM {
		// Read current raw.qemu so we can update sockets without clobbering other flags.
		curRawQEMU := ""
		if out, err := exec.Command("incus", "config", "get", name, "raw.qemu").Output(); err == nil {
			curRawQEMU = strings.TrimSpace(string(out))
		}
		newRawQEMU := updateRawQEMUSockets(curRawQEMU, cfg.CPUSockets)
		if newRawQEMU != curRawQEMU {
			// Use PATCH instead of lxc config set to avoid flag-parsing of '-smp ...' values.
			lxdPatchConfig(name, "raw.qemu", newRawQEMU)
		}
	}
	if err := applyConf("limits.memory", cfg.MemoryLimit); err != nil {
		return err
	}
	hugepagesVal := ""
	if cfg.MemoryHugepages {
		hugepagesVal = "true"
	}
	if err := applyConf("limits.memory.hugepages", hugepagesVal); err != nil {
		return err
	}
	if err := applyConf("user.memory_reservation", cfg.MemoryReservation); err != nil {
		return err
	}
	nestingVal := ""
	if cfg.Nesting {
		nestingVal = "true"
	}
	if err := applyConf("security.nesting", nestingVal); err != nil {
		return err
	}
	autostart := "false"
	if cfg.Autostart {
		autostart = "true"
	}
	if err := applyConf("boot.autostart", autostart); err != nil {
		return err
	}
	// user.zfsnas.force_running — when "true", ZNAS auto-restarts the
	// instance after an unexpected stop (guest poweroff / crash / OOM /
	// external CLI stop). Empty string clears the key.
	forceRunning := ""
	if cfg.ForceRunning {
		forceRunning = "true"
	}
	if err := applyConf("user.zfsnas.force_running", forceRunning); err != nil {
		return err
	}
	// migration.stateful — VM-only; controls whether QEMU is initialised in a way
	// that supports stateful (memory-including) snapshots. Can only be changed while
	// the instance is stopped; ignore the error here so other settings still apply
	// (the UI already warns the user about the stop requirement).
	//
	// Incus rules out three combinations entirely:
	//   1. TPM + migration.stateful=true (mutually exclusive)
	//   2. additional disks from a non-shared pool + migration.stateful=true
	//      (ZFS is local, so any extra disk attached on a ZFS-backed host
	//      breaks the start: "Only additional disks coming from a shared
	//      storage pool are supported with migration.stateful=true").
	// We force stateful off when either constraint is violated rather than
	// letting the VM end up in a state that cannot start.
	if cfg.IsVM {
		hasExtraDisks := false
		for _, d := range cfg.Disks {
			if !d.IsRoot {
				hasExtraDisks = true
				break
			}
		}
		if !hasExtraDisks && len(cfg.ExistingDisks) > 0 {
			hasExtraDisks = true
		}
		// CDROMs attached via Incus-native disk devices have an external
		// path source — Incus rejects them outright when migration.stateful
		// is true with "Only Incus-managed disks are allowed". So as soon
		// as the user attaches an installer ISO, we force stateful off.
		// Without this gate the VM either won't start at all or, if the
		// CDROM fails to attach, OVMF won't see the install medium and
		// the user sees "boot media not found" / PXE timeout.
		hasCDROMs := false
		if cfg.ApplyCDROMs {
			for _, p := range cfg.CDROMs {
				if p != "" {
					hasCDROMs = true
					break
				}
			}
		} else if cfg.ApplyCDROM && cfg.CDROMPath != "" {
			hasCDROMs = true
		}
		wantStateful := cfg.StatefulSnapshots && !cfg.TPM && !hasExtraDisks && !hasCDROMs
		migVal := "false"
		if wantStateful {
			migVal = "true"
		}
		applyConf("migration.stateful", migVal) //nolint: errcheck — best-effort
		// Raise the ZFS state dataset quota to at least RAM + 20% so stateful snapshots
		// can write the full memory image. LXD hard-codes the initial quota at 100 MiB.
		if wantStateful {
			lxdEnsureStateQuota(name)
		}
	}

	// Firmware / Secure Boot (VM-only).
	if cfg.IsVM && cfg.Firmware != "" {
		if cfg.Firmware == "bios" {
			applyConf("security.secureboot", "false") //nolint:errcheck
			applyConf("security.csm", "true")         //nolint:errcheck
		} else {
			applyConf("security.csm", "false") //nolint:errcheck
			sbVal := "true"
			if !cfg.SecureBoot {
				sbVal = "false"
			}
			if err := applyConf("security.secureboot", sbVal); err != nil {
				if !strings.Contains(err.Error(), "isn't supported") {
					return fmt.Errorf("secure boot: %w", err)
				}
				// LXD 6.x removed security.secureboot; use raw.qemu pflash flag instead.
				if err2 := lxdSetSecureBootRawQEMU(name, cfg.SecureBoot); err2 != nil {
					return fmt.Errorf("secure boot (raw.qemu fallback): %w", err2)
				}
			}
		}
	}
	// TPM device (VM-only): add or remove the tpm device based on cfg.TPM.
	if cfg.IsVM {
		hasTPM := false
		if out, err := exec.Command("incus", "query", "/1.0/instances/"+name).Output(); err == nil {
			var inst struct {
				Devices map[string]map[string]string `json:"devices"`
			}
			if json.Unmarshal(out, &inst) == nil {
				for _, d := range inst.Devices {
					if d["type"] == "tpm" {
						hasTPM = true
						break
					}
				}
			}
		}
		if cfg.TPM && !hasTPM {
			exec.Command("incus", "config", "device", "add", name, "tpm", "tpm").Run() //nolint:errcheck
		} else if !cfg.TPM && hasTPM {
			exec.Command("incus", "config", "device", "remove", name, "tpm").Run() //nolint:errcheck
		}
	}
	// Machine type (VM-only). Empty string unsets the override, letting LXD
	// choose. Incus 6.0.x lacks the qemu.machine.type config key — fall back
	// to a -machine TYPE clause inside raw.qemu when the native key is
	// rejected so the dropdown actually works on this Incus version.
	if cfg.IsVM {
		if err := applyConf("qemu.machine.type", cfg.MachineType); err != nil {
			out, _ := exec.Command("incus", "config", "get", name, "raw.qemu").Output()
			lxdPatchConfig(name, "raw.qemu",
				updateRawQEMUMachine(strings.TrimSpace(string(out)), cfg.MachineType))
		} else {
			// Native key accepted: clear any prior raw.qemu -machine override
			// so the two paths can't disagree on subsequent edits.
			out, _ := exec.Command("incus", "config", "get", name, "raw.qemu").Output()
			cleaned := updateRawQEMUMachine(strings.TrimSpace(string(out)), "")
			if strings.TrimSpace(string(out)) != cleaned {
				lxdPatchConfig(name, "raw.qemu", cleaned)
			}
		}
	}

	// SMBIOS types 1, 2, and 4 (VM-only). Stored inside raw.qemu's -smbios
	// clauses so values survive a stop/start cycle and round-trip through
	// the GET path. Always rewrite each clause — passing a nil/empty struct
	// strips any previously-attached clause of that type.
	if cfg.IsVM {
		out, _ := exec.Command("incus", "config", "get", name, "raw.qemu").Output()
		current := strings.TrimSpace(string(out))
		updated := updateRawQEMUSMBIOSType1(current, cfg.SMBIOS)
		updated = updateRawQEMUSMBIOSType2(updated, cfg.SMBIOSType2)
		updated = updateRawQEMUSMBIOSType4(updated, cfg.SMBIOSType4)
		if updated != current {
			lxdPatchConfig(name, "raw.qemu", updated)
		}
	}

	// Container-specific features (CPU throttle, swap, security, FUSE).
	// Skipped for VMs and when the frontend sends apply_container_features=false
	// (preserves backwards compatibility with older frontend versions).
	if cfg.ApplyContainerFeatures {
		allowance := ""
		if cfg.CPULimitPct > 0 && cfg.CPULimitPct <= 100 {
			allowance = fmt.Sprintf("%dms/100ms", cfg.CPULimitPct)
		}
		if err := applyConf("limits.cpu.allowance", allowance); err != nil {
			return err
		}
		priority := ""
		if cfg.CPUShares > 0 && cfg.CPUShares <= 10 {
			priority = strconv.Itoa(cfg.CPUShares)
		}
		if err := applyConf("limits.cpu.priority", priority); err != nil {
			return err
		}
		if err := applyConf("limits.memory.swap", normalizeSwapLimit(cfg.SwapLimit)); err != nil {
			return err
		}
		privVal := ""
		if !cfg.Unprivileged {
			privVal = "true"
		}
		if err := applyConf("security.privileged", privVal); err != nil {
			return err
		}
		// "Allow keyctl" must NOT be expressed via security.syscalls.allow:
		// that key is a *whitelist* — when set to "keyctl" LXD denies every
		// other syscall (close, read, write, ...) and the container can't
		// boot. Use the dedicated intercept key when LXD supports it; on
		// older LXD (5.0.x and below) the intercept key doesn't exist and
		// the default seccomp profile already permits keyctl, so we leave
		// the config untouched. Either way, drop any stale allow=keyctl
		// value left by the old buggy code.
		exec.Command("incus", "config", "unset", name, "security.syscalls.allow").Run() //nolint:errcheck
		if cfg.FeatureKeyctl {
			if lxdSupportsConfigKey("security.syscalls.intercept.keyctl") {
				if err := applyConf("security.syscalls.intercept.keyctl", "true"); err != nil {
					return err
				}
			}
		} else {
			exec.Command("incus", "config", "unset", name, "security.syscalls.intercept.keyctl").Run() //nolint:errcheck
		}
	}

	// Fetch current instance-level devices for diff.
	rawOut, err := exec.Command("incus", "query", "/1.0/instances/"+name).Output()
	if err != nil {
		return fmt.Errorf("query instance: %w", err)
	}
	var rawDev struct {
		Type            string                       `json:"type"`
		Status          string                       `json:"status"`
		Config          map[string]string            `json:"config"`
		Devices         map[string]map[string]string `json:"devices"`
		ExpandedDevices map[string]map[string]string `json:"expanded_devices"`
		ExpandedConfig  map[string]string            `json:"expanded_config"`
	}
	json.Unmarshal(rawOut, &rawDev)
	if rawDev.Devices == nil {
		rawDev.Devices = map[string]map[string]string{}
	}
	if rawDev.ExpandedDevices == nil {
		rawDev.ExpandedDevices = map[string]map[string]string{}
	}
	isVM := rawDev.Type == "virtual-machine"
	isRunning := rawDev.Status == "Running"

	// expandedNICs: all NICs visible to the instance (instance-level + profile).
	// Used to detect profile-inherited NICs that cannot be removed at the instance level.
	expandedNICs := map[string]map[string]string{}
	for n, d := range rawDev.ExpandedDevices {
		if d["type"] == "nic" {
			expandedNICs[n] = d
		}
	}

	curNICs := map[string]map[string]string{}
	curDisks := map[string]map[string]string{}
	curUSB := map[string]map[string]string{}
	curPCI := map[string]map[string]string{}
	curPassthrough := map[string]map[string]string{}
	for n, d := range rawDev.Devices {
		switch d["type"] {
		case "nic":
			curNICs[n] = d
		case "disk":
			curDisks[n] = d
		case "usb":
			curUSB[n] = d
		case "pci":
			curPCI[n] = d
		case "tpm":
			// TPM is managed separately via cfg.TPM; exclude from generic passthrough diff.
		default:
			curPassthrough[n] = d
		}
	}

	// nicBridgedArgs returns lxc args to add a NIC as nictype=bridged.
	nicBridgedArgs := func(devName, bridge string) []string {
		return []string{"config", "device", "add", name, devName, "nic", "nictype=bridged", "parent=" + bridge}
	}

	// lxcNICRun executes lxc args for a NIC operation on the given bridge.
	// If LXD returns a DNS name conflict (two NICs on the same LXD-managed bridge),
	// it disables DNS on that bridge (dns.mode=none) and retries once.
	lxcNICRun := func(bridge string, args []string) ([]byte, error) {
		out, err := exec.Command("incus", args...).CombinedOutput()
		if err != nil && strings.Contains(string(out), "DNS name") {
			exec.Command("incus", "network", "set", bridge, "dns.mode=none").Run() //nolint:errcheck
			out, err = exec.Command("incus", args...).CombinedOutput()
		}
		return out, err
	}

	// ── NIC diff ──────────────────────────────────────────────────────────────
	wantNICs := map[string]struct{}{}
	for _, nic := range cfg.NICs {
		if !lxdDevNameRe.MatchString(nic.Name) {
			return fmt.Errorf("invalid NIC name: %s", nic.Name)
		}
		wantNICs[nic.Name] = struct{}{}
		cur, exists := curNICs[nic.Name]
		_, inProfile := expandedNICs[nic.Name]
		isProfileOnly := !exists && inProfile
		if !exists {
			if !nic.Connected {
				// Profile NIC being disconnected: bring the link down inside the container.
				// Instance-level NICs that don't exist yet just skip (nothing to disconnect).
				if isProfileOnly && isRunning {
					exec.Command("incus", "exec", name, "--", "ip", "link", "set", nic.Name, "down").Run()
				}
				continue
			}
			if isProfileOnly {
				// Profile-inherited NIC. Compare the requested settings against the
				// profile's effective values; if anything the user can edit (bridge,
				// VLAN, MAC) differs we must add a local instance-level device that
				// overrides the profile NIC. Without this override the request was
				// silently swallowed — the audit log showed "lxd_edit_config" success
				// while the VM kept the profile's old bridge.
				profDev := expandedNICs[nic.Name]
				profBridge := profDev["network"]
				if profBridge == "" {
					profBridge = profDev["parent"]
				}
				profVlan := profDev["vlan"]
				profMAC := strings.ToLower(profDev["hwaddr"])
				// Same volatile-MAC fallback as the instance-NIC path below:
				// the GET response synthesises an effective MAC from
				// volatile.<name>.hwaddr when the profile / device hwaddr
				// is unset, and a no-op save would round-trip that value
				// here. Without this fallback every save would create a
				// per-instance override device just to pin the volatile MAC.
				if profMAC == "" {
					profMAC = strings.ToLower(rawDev.ExpandedConfig["volatile."+nic.Name+".hwaddr"])
				}
				wantVlan := ""
				if nic.VlanID > 0 {
					wantVlan = fmt.Sprintf("%d", nic.VlanID)
				}
				wantMAC := strings.ToLower(nic.MAC)
				needsOverride := profBridge != nic.Bridge || profVlan != wantVlan || profMAC != wantMAC
				if needsOverride {
					// Always pin overrides as nictype=bridged so we don't end up
					// double-registering the instance in an LXD-managed bridge's DNS.
					args := nicBridgedArgs(nic.Name, nic.Bridge)
					if wantVlan != "" {
						args = append(args, "vlan="+wantVlan)
					}
					if wantMAC != "" {
						args = append(args, "hwaddr="+wantMAC)
					}
					if out, err := lxcNICRun(nic.Bridge, args); err != nil {
						return fmt.Errorf("override profile NIC %s: %s", nic.Name, strings.TrimSpace(string(out)))
					}
				}
				// Bring the link up regardless (best-effort; VMs without lxd-agent
				// will fail silently, which is the existing pre-fix behaviour).
				if isRunning {
					exec.Command("incus", "exec", name, "--", "ip", "link", "set", nic.Name, "up").Run()
				}
				continue
			}
			// Truly new NIC (or reconnect of a previously disconnected NIC).
			// Always add as nictype=bridged to avoid LXD DNS registration.
			args := nicBridgedArgs(nic.Name, nic.Bridge)
			if nic.VlanID > 0 {
				args = append(args, fmt.Sprintf("vlan=%d", nic.VlanID))
			}
			if nic.MAC != "" {
				args = append(args, "hwaddr="+strings.ToLower(nic.MAC))
			}
			if out, err := lxcNICRun(nic.Bridge, args); err != nil {
				return fmt.Errorf("add NIC %s: %s", nic.Name, strings.TrimSpace(string(out)))
			}
			// Clear any disconnected-NIC metadata for this device.
			exec.Command("incus", "config", "unset", name, "user.disconnected_nics."+nic.Name).Run() //nolint:errcheck
		} else {
			// NIC exists in instance config.
			// Clear any stale disconnected-NIC metadata so the UI stays consistent.
			exec.Command("incus", "config", "unset", name, "user.disconnected_nics."+nic.Name).Run() //nolint:errcheck

			curUsesNetwork := cur["network"] != "" // "network=" style registers with LXD DNS
			curBridge := cur["network"]
			if curBridge == "" {
				curBridge = cur["parent"]
			}

			if !nic.Connected {
				// Disconnect: remove the LXD device and save NIC info as instance metadata
				// so the UI can show it as disconnected and restore it on reconnect.
				if isRunning {
					return fmt.Errorf("stop the VM first to disconnect NIC %s", nic.Name)
				}
				if out, err := exec.Command("incus", "config", "device", "remove", name, nic.Name).CombinedOutput(); err != nil {
					return fmt.Errorf("disconnect NIC %s: %s", nic.Name, strings.TrimSpace(string(out)))
				}
				mac := cur["hwaddr"]
				if mac == "" {
					mac = rawDev.ExpandedConfig["volatile."+nic.Name+".hwaddr"]
				}
				metaVal, _ := json.Marshal(map[string]string{
					"bridge": curBridge,
					"mac":    mac,
					"vlan":   cur["vlan"],
				})
				exec.Command("incus", "config", "set", name,
					"user.disconnected_nics."+nic.Name, string(metaVal)).Run() //nolint:errcheck
				// wantNICs still contains this name so the deletion loop won't retry the remove.
				continue
			}

			bridgeChanged := curBridge != nic.Bridge

			if curUsesNetwork && bridgeChanged {
				// NIC is "network=" style and bridge is changing.
				// "device set" cannot mix nictype= and network= properties, so must remove+re-add.
				if isRunning {
					return fmt.Errorf("stop the VM first to change NIC %s bridge (managed-network NIC requires restart)", nic.Name)
				}
				if out, err := exec.Command("incus", "config", "device", "remove", name, nic.Name).CombinedOutput(); err != nil {
					return fmt.Errorf("update NIC %s (remove): %s", nic.Name, strings.TrimSpace(string(out)))
				}
				addArgs := nicBridgedArgs(nic.Name, nic.Bridge)
				if nic.VlanID > 0 {
					addArgs = append(addArgs, fmt.Sprintf("vlan=%d", nic.VlanID))
				}
				if nic.MAC != "" {
					addArgs = append(addArgs, "hwaddr="+strings.ToLower(nic.MAC))
				}
				if out, err := lxcNICRun(nic.Bridge, addArgs); err != nil {
					return fmt.Errorf("update NIC %s (re-add): %s", nic.Name, strings.TrimSpace(string(out)))
				}
				continue // VLAN and MAC already applied above
			} else if !curUsesNetwork && bridgeChanged {
				// nictype=bridged parent= style; change parent. Works on running VMs.
				setArgs := []string{"config", "device", "set", name, nic.Name,
					"nictype=bridged", "parent=" + nic.Bridge}
				if out, err := lxcNICRun(nic.Bridge, setArgs); err != nil {
					return fmt.Errorf("update NIC %s: %s", nic.Name, strings.TrimSpace(string(out)))
				}
			}
			// If curUsesNetwork && !bridgeChanged: same bridge, leave as-is; fall through to patch.

			// Patch VLAN and MAC in place.
			curVlan := cur["vlan"]
			wantVlan := ""
			if nic.VlanID > 0 {
				wantVlan = fmt.Sprintf("%d", nic.VlanID)
			}
			if curVlan != wantVlan {
				if wantVlan == "" {
					exec.Command("incus", "config", "device", "unset", name, nic.Name, "vlan").Run() //nolint:errcheck
				} else {
					if out, err := exec.Command("incus", "config", "device", "set",
						name, nic.Name, "vlan="+wantVlan).CombinedOutput(); err != nil {
						return fmt.Errorf("update NIC %s vlan: %s", nic.Name, strings.TrimSpace(string(out)))
					}
				}
			}
			// MAC comparison: the GET endpoint returns the effective MAC by
			// falling back to volatile.<nic>.hwaddr when the device-level
			// hwaddr is unset (Incus auto-assigns volatile MACs). A round-
			// trip through the edit form therefore gives us wantMAC == the
			// volatile MAC even when the user changed nothing. If we only
			// compared against cur["hwaddr"] here, every save would re-pin
			// the MAC at the device level and Incus would treat that as a
			// NIC change — which on stateful or freshly-created VMs surfaces
			// as "Failed to detach NIC after 10s". Treat the volatile MAC
			// as part of the current state so a no-op save stays a no-op.
			curMAC := strings.ToLower(cur["hwaddr"])
			if curMAC == "" {
				curMAC = strings.ToLower(rawDev.ExpandedConfig["volatile."+nic.Name+".hwaddr"])
			}
			wantMAC := strings.ToLower(nic.MAC)
			if curMAC != wantMAC {
				if wantMAC == "" {
					exec.Command("incus", "config", "device", "unset", name, nic.Name, "hwaddr").Run() //nolint:errcheck
				} else {
					if out, err := exec.Command("incus", "config", "device", "set",
						name, nic.Name, "hwaddr="+wantMAC).CombinedOutput(); err != nil {
						return fmt.Errorf("update NIC %s hwaddr: %s", nic.Name, strings.TrimSpace(string(out)))
					}
				}
			}
		}
	}
	for n := range curNICs {
		if _, ok := wantNICs[n]; !ok {
			if out, err := exec.Command("incus", "config", "device", "remove", name, n).CombinedOutput(); err != nil {
				outStr := strings.TrimSpace(string(out))
				if isRunning {
					return fmt.Errorf("remove NIC %s: %s (stop the VM first to remove NICs)", n, outStr)
				}
				return fmt.Errorf("remove NIC %s: %s", n, outStr)
			}
		}
	}
	// Bring newly-added NICs up. Disconnected NICs are removed from LXD config entirely.
	if isRunning {
		for _, nic := range cfg.NICs {
			if nic.Connected {
				exec.Command("incus", "exec", name, "--", "ip", "link", "set", nic.Name, "up").Run() //nolint:errcheck
			}
		}
	}

	// Apply static IP config for container NICs when IPv4Mode is set to "static".
	if !isVM {
		var staticNICs []LXDNIC
		for _, nic := range cfg.NICs {
			if nic.IPv4Mode != "static" || nic.IPv4Addr == "" {
				continue
			}
			n := LXDNIC{IPv4Mode: nic.IPv4Mode, IPv4Addr: nic.IPv4Addr, IPv4GW: nic.IPv4GW, DNS1: nic.DNS1, DNS2: nic.DNS2}
			_pushStaticNetworkConfig(name, nic.Name, n)
			if isRunning {
				_applyStaticIPCommands(name, nic.Name, n)
			}
			staticNICs = append(staticNICs, n)
		}
		if isRunning {
			if dnsLines := _collectDNSLines(staticNICs); len(dnsLines) > 0 {
				resolvConf := strings.Join(dnsLines, "\n") + "\n"
				cmd := exec.Command("incus", "exec", name, "--", "/bin/sh", "-c",
					"rm -f /etc/resolv.conf && cat > /etc/resolv.conf")
				cmd.Stdin = strings.NewReader(resolvConf)
				cmd.Run()
			}
		}
	}

	// ── Disk diff ─────────────────────────────────────────────────────────────
	wantDisks := map[string]struct{}{}
	for _, disk := range cfg.Disks {
		if !lxdDevNameRe.MatchString(disk.Name) {
			return fmt.Errorf("invalid disk name: %s", disk.Name)
		}
		wantDisks[disk.Name] = struct{}{}
		cur, exists := curDisks[disk.Name]
		if !exists {
			// LXD 5.x requires a named block volume; create it first then attach.
			volName := name + "-" + disk.Name
			volArgs := []string{"storage", "volume", "create", disk.Pool, volName, "--type", "block"}
			if disk.Size != "" {
				volArgs = append(volArgs, "size="+disk.Size)
			}
			if out, err := exec.Command("incus", volArgs...).CombinedOutput(); err != nil {
				return fmt.Errorf("create volume for %s: %s", disk.Name, strings.TrimSpace(string(out)))
			}
			devArgs := []string{"config", "device", "add", name, disk.Name, "disk",
				"pool=" + disk.Pool, "source=" + volName}
			if out, err := exec.Command("incus", devArgs...).CombinedOutput(); err != nil {
				exec.Command("incus", "storage", "volume", "delete", disk.Pool, volName).Run()
				return fmt.Errorf("add disk %s: %s", disk.Name, strings.TrimSpace(string(out)))
			}
			// Apply ZFS reservation for the newly created volume.
			if zfsPath := lxdFindZFSVol(volName); zfsPath != "" {
				lxdSetZvolReservation(zfsPath, disk.ReservePct)
			}
		} else if disk.IsRoot && disk.Size != "" && cur["size"] != disk.Size {
			// Only root disks support size quota via LXD device config.
			// Non-root custom volumes are resized via ZFS zvol directly.
			//
			// Compare in bytes, not as strings: at create time we pass the
			// 16K-aligned bare-bytes form (e.g. "20000008192B") so the volsize
			// fits the ZFS volblocksize, and the UI round-trips that as
			// "20GB" (≈ 19,999,991,808 bytes off — still strictly smaller).
			// Without this check, simply re-saving the VM (e.g. to update the
			// description) would call `lxc config device set … size=20GB`,
			// which LXD rejects with "Block volumes cannot be shrunk".
			curBytes := lxdVolSizeBytes(cur["size"])
			newBytes := lxdVolSizeBytes(disk.Size)
			if newBytes > 0 && curBytes > 0 && newBytes <= curBytes {
				// Same size or smaller — skip. ZFS volumes can only grow.
			} else if out, err := exec.Command("incus", "config", "device", "set", name, disk.Name, "size", disk.Size).CombinedOutput(); err != nil {
				return fmt.Errorf("resize disk %s: %s", disk.Name, strings.TrimSpace(string(out)))
			}
		} else if !disk.IsRoot && disk.Size != "" && cur["size"] == "" {
			// Non-root custom volume: grow the ZFS zvol if the user increased the size.
			// Compare raw bytes to avoid GiB/GB unit ambiguity with `zfs set`.
			// Only allow growing — ZFS does not support shrinking zvols safely.
			wantBytes := lxdVolSizeBytes(disk.Size)
			if wantBytes > 0 {
				volName := cur["source"]
				if volName == "" {
					volName = name + "-" + disk.Name
				}
				if zfsPath := lxdFindZFSVol(volName); zfsPath != "" {
					var currentBytes int64
					if out, err := exec.Command("zfs", "get", "-Hp", "volsize", zfsPath).Output(); err == nil {
						fields := strings.Fields(strings.TrimSpace(string(out)))
						if len(fields) >= 3 {
							currentBytes, _ = strconv.ParseInt(fields[2], 10, 64)
						}
					}
					if wantBytes > currentBytes {
						exec.Command("sudo", "zfs", "set", fmt.Sprintf("volsize=%d", wantBytes), zfsPath).Run()
					}
				}
			}
		}
		// Apply ZFS reservation for all existing zvol disks (root and non-root).
		if exists {
			var zfsPath string
			if disk.IsRoot {
				// Reconstruct root disk ZFS path — not carried in the PUT payload.
				if zfsPool := lxdZFSPoolForLXDPool(disk.Pool); zfsPool != "" {
					if isVM {
						zfsPath = zfsPool + "/virtual-machines/" + name + ".block"
					} else {
						zfsPath = zfsPool + "/containers/" + name
					}
				}
			} else {
				volName := cur["source"]
				if volName == "" {
					volName = name + "-" + disk.Name
				}
				zfsPath = lxdFindZFSVol(volName)
			}
			if zfsPath != "" {
				sizeStr := cur["size"]
				if sizeStr == "" {
					sizeStr = disk.Size
				}
				lxdSetZvolReservation(zfsPath, disk.ReservePct)
			}
		}
		// Apply boot.priority change for existing disks.
		if exists {
			curPrio := cur["boot.priority"]
			if disk.BootPriority != curPrio {
				if disk.BootPriority == "" {
					exec.Command("incus", "config", "device", "unset", name, disk.Name, "boot.priority").Run()
				} else {
					exec.Command("incus", "config", "device", "set", name, disk.Name, "boot.priority", disk.BootPriority).Run()
				}
			}
			// Per-disk bus override beats the VM-wide DiskBus when non-empty.
			// Skip silently when LXD doesn't ship the disk_io_bus extension
			// (5.0.x) — `lxc config device set io.bus=…` would error and the
			// frontend already disables that field for unsupported daemons.
			if isVM && caps.DiskIOBus {
				want := disk.IOBus
				if want == "" {
					want = cfg.DiskBus
				}
				if want != cur["io.bus"] {
					if want == "" {
						exec.Command("incus", "config", "device", "unset", name, disk.Name, "io.bus").Run() //nolint:errcheck
					} else {
						exec.Command("incus", "config", "device", "set", name, disk.Name, "io.bus", want).Run() //nolint:errcheck
					}
				}
			}
			// io.cache (LXD ≥ 5.0 with disk_io_cache extension; widely available).
			if isVM && caps.DiskIOCache && disk.IOCache != cur["io.cache"] {
				if disk.IOCache == "" {
					exec.Command("incus", "config", "device", "unset", name, disk.Name, "io.cache").Run() //nolint:errcheck
				} else {
					exec.Command("incus", "config", "device", "set", name, disk.Name, "io.cache", disk.IOCache).Run() //nolint:errcheck
				}
			}
			// readonly: skip on root disks (LXD rejects readonly=true on /).
			if !disk.IsRoot {
				curRO := cur["readonly"] == "true"
				if curRO != disk.ReadOnly {
					if disk.ReadOnly {
						exec.Command("incus", "config", "device", "set", name, disk.Name, "readonly", "true").Run() //nolint:errcheck
					} else {
						exec.Command("incus", "config", "device", "unset", name, disk.Name, "readonly").Run() //nolint:errcheck
					}
				}
			}
		}
		// For newly added disks, apply per-disk knobs immediately after device add (VM-only).
		if !exists && isVM {
			if caps.DiskIOBus {
				want := disk.IOBus
				if want == "" {
					want = cfg.DiskBus
				}
				if want != "" {
					exec.Command("incus", "config", "device", "set", name, disk.Name, "io.bus", want).Run() //nolint:errcheck
				}
			}
			if caps.DiskIOCache && disk.IOCache != "" {
				exec.Command("incus", "config", "device", "set", name, disk.Name, "io.cache", disk.IOCache).Run() //nolint:errcheck
			}
			if !disk.IsRoot && disk.ReadOnly {
				exec.Command("incus", "config", "device", "set", name, disk.Name, "readonly", "true").Run() //nolint:errcheck
			}
		}
	}
	detachOnly := map[string]bool{}
	for _, n := range cfg.DetachDisks {
		detachOnly[n] = true
	}
	for n, d := range curDisks {
		if d["path"] == "/" {
			continue // never auto-remove root disk
		}
		if d["readonly"] == "true" && strings.HasSuffix(strings.ToLower(d["source"]), ".iso") {
			continue // cdrom handled separately below
		}
		if _, ok := wantDisks[n]; !ok {
			volPool := d["pool"]
			volName := d["source"]
			if out, err := exec.Command("incus", "config", "device", "remove", name, n).CombinedOutput(); err != nil {
				return fmt.Errorf("remove disk %s: %s", n, strings.TrimSpace(string(out)))
			}
			// Delete the backing block volume unless this is a detach-only operation.
			if !detachOnly[n] && volPool != "" && volName == name+"-"+n {
				exec.Command("incus", "storage", "volume", "delete", volPool, volName).Run()
			}
		}
	}

	// Apply DiskBus to the root disk independently of the cfg.Disks loop.
	// Root disks are typically profile-inherited (absent from cfg.Disks/curDisks) and
	// cannot be changed while the VM is running (LXD rejects hotplug for root disks).
	// If the VM is running we stop it gracefully, apply, then restart.
	if isVM && caps.DiskIOBus {
		rootName := ""
		for n, d := range rawDev.ExpandedDevices {
			if d["type"] == "disk" && d["path"] == "/" {
				rootName = n
				break
			}
		}
		if rootName != "" {
			_, rootIsInstance := curDisks[rootName]
			curBus := rawDev.ExpandedDevices[rootName]["io.bus"]
			if cfg.DiskBus != curBus {
				// For instance-level root disks on running VMs: 'device set' saves the config even
				// when hotplug fails — the change takes effect on next restart.
				// For profile-inherited root disks on running VMs: 'device override' does NOT save
				// on hotplug failure — must stop, apply, then restart.
				needStop := !rootIsInstance && isRunning
				if needStop {
					// Portal-initiated stop — suppress the unexpected-stop alert.
					MarkUserInitiatedStop(name)
					if err := exec.Command("incus", "stop", name, "--timeout=20").Run(); err != nil {
						exec.Command("incus", "stop", name, "--force").Run() //nolint:errcheck
					}
				}
				if cfg.DiskBus == "" {
					exec.Command("incus", "config", "device", "unset", name, rootName, "io.bus").Run() //nolint:errcheck
				} else if rootIsInstance {
					exec.Command("incus", "config", "device", "set", name, rootName, "io.bus", cfg.DiskBus).Run() //nolint:errcheck
				} else {
					// Profile-inherited root: 'override' creates an instance-level copy with io.bus set.
					exec.Command("incus", "config", "device", "override", name, rootName, "io.bus="+cfg.DiskBus).Run() //nolint:errcheck
				}
				if needStop {
					exec.Command("incus", "start", name).Run() //nolint:errcheck
				}
			}
		}
	}

	// ── CDROM diff ────────────────────────────────────────────────────────────
	// For VMs, CDROMs are attached via raw.qemu (if=ide → ICH9 AHCI/SATA in Q35)
	// so the Windows installer sees them without any VirtIO drivers.
	// For containers, CDROMs are attached as LXD disk devices (loop-mounted).
	if cfg.ApplyCDROMs {
		if cfg.IsVM {
			vmApplyCDROMs(name, cfg.CDROMs, applyConf)
		} else {
			// Container path: use LXD disk devices directly.
			for n, d := range rawDev.Devices {
				if d["type"] == "disk" && d["readonly"] == "true" && strings.HasSuffix(strings.ToLower(d["source"]), ".iso") {
					exec.Command("incus", "config", "device", "remove", name, n).Run() //nolint:errcheck
				}
			}
			for i, path := range cfg.CDROMs {
				if path == "" || !filepath.IsAbs(path) {
					continue
				}
				devName := fmt.Sprintf("cdrom%d", i)
				exec.Command("incus", "config", "device", "add", name, devName, "disk", //nolint:errcheck
					"source="+path, "readonly=true").Run()
			}
		}
	} else if cfg.ApplyCDROM {
		// Legacy single-drive path.
		if cfg.IsVM {
			paths := []string{}
			if cfg.CDROMPath != "" {
				paths = []string{cfg.CDROMPath}
			}
			vmApplyCDROMs(name, paths, applyConf)
		} else {
			for n, d := range rawDev.Devices {
				if d["type"] == "disk" && d["readonly"] == "true" && strings.HasSuffix(strings.ToLower(d["source"]), ".iso") {
					exec.Command("incus", "config", "device", "remove", name, n).Run() //nolint:errcheck
					break
				}
			}
			if cfg.CDROMPath != "" {
				exec.Command("incus", "config", "device", "add", name, "cdrom", "disk", //nolint:errcheck
					"source="+cfg.CDROMPath, "readonly=true").Run()
			}
		}
	}

	// ── Attach existing ZVol / LXD-managed volumes ───────────────────────────
	for _, ed := range cfg.ExistingDisks {
		devName := ed.DeviceName
		if devName == "" || !lxdDevNameRe.MatchString(devName) {
			continue
		}
		if _, alreadyExists := rawDev.Devices[devName]; alreadyExists {
			continue
		}
		var dArgs []string
		if pool, vol, ok := parseLXDVolRef(ed.DevPath); ok {
			dArgs = []string{"config", "device", "add", name, devName, "disk",
				"pool=" + pool, "source=" + vol}
		} else if filepath.IsAbs(ed.DevPath) {
			dArgs = []string{"config", "device", "add", name, devName, "disk",
				"source=" + ed.DevPath}
		} else {
			continue
		}
		if out, err := exec.Command("incus", dArgs...).CombinedOutput(); err != nil {
			return fmt.Errorf("attach existing disk %s: %s", devName, strings.TrimSpace(string(out)))
		}
	}

	// ── USB passthrough diff ───────────────────────────────────────────────────
	wantUSB := map[string]struct{}{}
	for _, usb := range cfg.USBDevices {
		if !lxdDevNameRe.MatchString(usb.DeviceName) {
			return fmt.Errorf("invalid USB device name: %s", usb.DeviceName)
		}
		if !usbIDRe.MatchString(usb.VendorID) || !usbIDRe.MatchString(usb.ProductID) {
			return fmt.Errorf("invalid USB IDs for device %s", usb.DeviceName)
		}
		wantUSB[usb.DeviceName] = struct{}{}
		cur, exists := curUSB[usb.DeviceName]
		if !exists || cur["vendorid"] != usb.VendorID || cur["productid"] != usb.ProductID {
			if exists {
				exec.Command("incus", "config", "device", "remove", name, usb.DeviceName).Run()
			}
			args := []string{"config", "device", "add", name, usb.DeviceName, "usb",
				"vendorid=" + usb.VendorID, "productid=" + usb.ProductID}
			if out, err := exec.Command("incus", args...).CombinedOutput(); err != nil {
				return fmt.Errorf("add USB %s: %s", usb.DeviceName, strings.TrimSpace(string(out)))
			}
		}
	}
	for n := range curUSB {
		if _, ok := wantUSB[n]; !ok {
			exec.Command("incus", "config", "device", "remove", name, n).Run()
		}
	}

	// ── PCI passthrough diff ───────────────────────────────────────────────────
	wantPCI := map[string]struct{}{}
	for _, pci := range cfg.PCIDevices {
		if !lxdDevNameRe.MatchString(pci.DeviceName) {
			return fmt.Errorf("invalid PCI device name: %s", pci.DeviceName)
		}
		if !pciAddrRe.MatchString(pci.Address) {
			return fmt.Errorf("invalid PCI address for device %s", pci.DeviceName)
		}
		addr := normPCIAddr(pci.Address)
		wantPCI[pci.DeviceName] = struct{}{}
		cur, exists := curPCI[pci.DeviceName]
		if !exists || normPCIAddr(cur["address"]) != addr {
			if exists {
				exec.Command("incus", "config", "device", "remove", name, pci.DeviceName).Run()
			}
			args := []string{"config", "device", "add", name, pci.DeviceName, "pci", "address=" + addr}
			if out, err := exec.Command("incus", args...).CombinedOutput(); err != nil {
				return fmt.Errorf("add PCI %s: %s", pci.DeviceName, strings.TrimSpace(string(out)))
			}
		}
	}
	for n := range curPCI {
		if _, ok := wantPCI[n]; !ok {
			exec.Command("incus", "config", "device", "remove", name, n).Run()
		}
	}

	// ── Generic passthrough diff (containers) ─────────────────────────────────
	wantPT := map[string]struct{}{}
	for _, dev := range cfg.PassthroughDevices {
		if !lxdDevNameRe.MatchString(dev.DeviceName) {
			return fmt.Errorf("invalid device name: %s", dev.DeviceName)
		}
		wantPT[dev.DeviceName] = struct{}{}
		if _, exists := curPassthrough[dev.DeviceName]; exists {
			exec.Command("incus", "config", "device", "remove", name, dev.DeviceName).Run()
		}
		args := []string{"config", "device", "add", name, dev.DeviceName, dev.Type}
		if dev.HostPath != "" {
			args = append(args, "path="+dev.HostPath)
		}
		for k, v := range dev.Extra {
			args = append(args, k+"="+v)
		}
		if out, err := exec.Command("incus", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("add device %s: %s", dev.DeviceName, strings.TrimSpace(string(out)))
		}
	}
	for n := range curPassthrough {
		if _, ok := wantPT[n]; !ok {
			exec.Command("incus", "config", "device", "remove", name, n).Run()
		}
	}

	// FUSE device add/remove for containers (tracked separately from generic passthrough).
	if !isVM && cfg.ApplyContainerFeatures {
		fuseExists := false
		for n, d := range rawDev.Devices {
			if n == "fuse" && d["type"] == "unix-char" && d["path"] == "/dev/fuse" {
				fuseExists = true
				break
			}
		}
		if cfg.FeatureFUSE && !fuseExists {
			exec.Command("incus", "config", "device", "add", name, "fuse", "unix-char", "path=/dev/fuse").Run()
		} else if !cfg.FeatureFUSE && fuseExists {
			exec.Command("incus", "config", "device", "remove", name, "fuse").Run()
		}
	}

	// Apply rombar/x-vga/aer via raw.qemu (LXD pci device type does not accept them directly).
	applyPCIRawQEMU(name, cfg.PCIDevices)

	// Apply "Disable virtual VGA adapter" via raw.qemu.conf + user key.
	// Toggling the checkbox in the Edit modal flips both in sync so the
	// override body and the state flag agree. Other raw.qemu.conf
	// content the user may have set (their own QEMU overrides) is
	// preserved by the regex strip in applyDisableVirtualVGA.
	curRawConf, _ := exec.Command("incus", "config", "get", name, "raw.qemu.conf").Output()
	newRawConf := applyDisableVirtualVGA(strings.TrimSpace(string(curRawConf)), cfg.DisableVirtualVGA)
	if strings.TrimSpace(newRawConf) != strings.TrimSpace(string(curRawConf)) {
		if newRawConf == "" {
			exec.Command("incus", "config", "unset", name, "raw.qemu.conf").Run() //nolint:errcheck
		} else {
			lxdPatchConfig(name, "raw.qemu.conf", newRawConf)
		}
	}
	if cfg.DisableVirtualVGA {
		exec.Command("incus", "config", "set", name, disableVGAUserKey+"=true").Run() //nolint:errcheck
	} else {
		exec.Command("incus", "config", "unset", name, disableVGAUserKey).Run() //nolint:errcheck
	}

	return nil
}

// ListHostBridges returns bridge interface names visible to the OS.
func ListHostBridges() ([]string, error) {
	out, err := exec.Command("ip", "-j", "link", "show", "type", "bridge").Output()
	if err != nil {
		return nil, err
	}
	var links []struct {
		IfName string `json:"ifname"`
	}
	if err := json.Unmarshal(out, &links); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(links))
	for _, l := range links {
		names = append(names, l.IfName)
	}
	return names, nil
}

// MachineVersions holds the QEMU machine type versions supported by this host.
type MachineVersions struct {
	I440FX []string `json:"i440fx"`
	Q35    []string `json:"q35"`
}

// GetLXDMachineVersions queries qemu-system-x86_64 for versioned machine types
// supported by the host (e.g. "pc-i440fx-9.1", "pc-q35-9.1").
// Lists are sorted newest-first.
func GetLXDMachineVersions() (MachineVersions, error) {
	// -machine help exits with status 1 but still writes to stdout/stderr.
	out, _ := exec.Command("qemu-system-x86_64", "-machine", "help").CombinedOutput()
	if len(out) == 0 {
		return MachineVersions{}, fmt.Errorf("qemu-system-x86_64 not available")
	}
	var mv MachineVersions
	i440Re := regexp.MustCompile(`^(pc-i440fx-\d+\.\d+)\s`)
	q35Re := regexp.MustCompile(`^(pc-q35-\d+\.\d+)\s`)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if m := i440Re.FindStringSubmatch(line); m != nil {
			mv.I440FX = append(mv.I440FX, m[1])
		} else if m := q35Re.FindStringSubmatch(line); m != nil {
			mv.Q35 = append(mv.Q35, m[1])
		}
	}
	return mv, nil
}

// LXDListProfiles lists LXD profile names.
func LXDListProfiles() ([]string, error) {
	out, err := exec.Command("incus", "profile", "list", "--format", "json").Output()
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	names := make([]string, len(raw))
	for i, r := range raw {
		names[i] = r.Name
	}
	return names, nil
}

// LXDListStoragePools lists LXD storage pool names.
func LXDListStoragePools() ([]string, error) {
	out, err := exec.Command("incus", "storage", "list", "--format", "json").Output()
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	names := make([]string, len(raw))
	for i, r := range raw {
		names[i] = r.Name
	}
	return names, nil
}

// LXDNetworkInfo describes one LXD bridge network.
type LXDNetworkInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Managed     bool   `json:"managed"`
}

// LXDListNetworks lists LXD bridge network names.
func LXDListNetworks() ([]string, error) {
	infos, err := LXDListNetworkInfos()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(infos))
	for _, i := range infos {
		names = append(names, i.Name)
	}
	return names, nil
}

// LXDListNetworkInfos lists LXD bridge networks with descriptions.
func LXDListNetworkInfos() ([]LXDNetworkInfo, error) {
	out, err := exec.Command("incus", "network", "list", "--format", "json").Output()
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Name        string `json:"name"`
		Type        string `json:"type"`
		Description string `json:"description"`
		Managed     bool   `json:"managed"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	var infos []LXDNetworkInfo
	for _, r := range raw {
		if r.Type == "bridge" {
			infos = append(infos, LXDNetworkInfo{Name: r.Name, Description: r.Description, Managed: r.Managed})
		}
	}
	return infos, nil
}

// LXDRemote represents a configured LXD/Incus image remote.
type LXDRemote struct {
	Name     string `json:"name"`
	URL      string `json:"url"`
	Protocol string `json:"protocol"`
}

// LXDListRemotes returns all configured image remotes, excluding the local LXD daemon.
// lxc remote list --format json returns a map[name]remote (not an array), with "addr" for the URL.
func LXDListRemotes() ([]LXDRemote, error) {
	out, err := exec.Command("incus", "remote", "list", "--format", "json").Output()
	if err != nil {
		return nil, fmt.Errorf("lxc remote list: %w", err)
	}
	// lxc remote list --format json returns a map: { "name": { "addr": url, "protocol": ... }, ... }
	var raw map[string]struct {
		Addr     string `json:"addr"`
		Protocol string `json:"protocol"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("lxc remote list parse: %w", err)
	}
	// Sort names for stable ordering
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)
	remotes := make([]LXDRemote, 0, len(names))
	for _, name := range names {
		r := raw[name]
		if r.Protocol == "lxd" || r.Protocol == "incus" || name == "local" {
			continue
		}
		remotes = append(remotes, LXDRemote{Name: name, URL: r.Addr, Protocol: r.Protocol})
	}
	return remotes, nil
}

// LXDListRemoteImages lists images from a remote (e.g. "images:"), filtered by kind.
// kind is "virtual-machine" or "container".
func LXDListRemoteImages(remote, kind string) ([]LXDImage, error) {
	args := []string{"image", "list", remote, "--format", "json"}
	if kind != "" {
		args = append(args, "type="+kind)
	}
	out, err := exec.Command("incus", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("lxc image list: %w", err)
	}

	var raw []struct {
		Fingerprint string            `json:"fingerprint"`
		Properties  map[string]string `json:"properties"`
		Type        string            `json:"type"`
		Size        int64             `json:"size"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}

	imgs := make([]LXDImage, 0, len(raw))
	for _, r := range raw {
		imgs = append(imgs, LXDImage{
			Fingerprint: r.Fingerprint,
			Description: r.Properties["description"],
			OS:          r.Properties["os"],
			Version:     r.Properties["release"],
			Arch:        r.Properties["architecture"],
			Type:        r.Type,
			Size:        r.Size,
			Variant:     r.Properties["variant"],
			Serial:      r.Properties["serial"],
		})
	}
	return imgs, nil
}

// LXDListLocalImages lists images already present in the local LXD image store.
func LXDListLocalImages(kind string) ([]LXDImage, error) {
	args := []string{"image", "list", "--format", "json"}
	if kind != "" {
		args = append(args, "type="+kind)
	}
	out, err := exec.Command("incus", args...).Output()
	if err != nil {
		return nil, fmt.Errorf("lxc image list: %w", err)
	}
	var raw []struct {
		Fingerprint  string `json:"fingerprint"`
		Architecture string `json:"architecture"`
		Type         string `json:"type"`
		Size         int64  `json:"size"`
		Aliases      []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		} `json:"aliases"`
		Properties map[string]string `json:"properties"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	imgs := make([]LXDImage, 0, len(raw))
	for _, r := range raw {
		desc := r.Properties["description"]
		if desc == "" && len(r.Aliases) > 0 {
			desc = r.Aliases[0].Description
			if desc == "" {
				desc = r.Aliases[0].Name
			}
		}
		if desc == "" {
			desc = r.Fingerprint[:12]
		}
		imgs = append(imgs, LXDImage{
			Fingerprint: r.Fingerprint,
			Description: desc,
			OS:          r.Properties["os"],
			Version:     r.Properties["release"],
			Arch:        r.Architecture,
			Type:        r.Type,
			Size:        r.Size,
			Variant:     r.Properties["variant"],
			Serial:      r.Properties["serial"],
		})
	}
	return imgs, nil
}

// LXDCreateVM creates a virtual machine according to the request, writing
// progress lines to logCh.
func LXDCreateVM(req LXDCreateVMRequest, logCh chan<- string) error {
	if !lxdNameRe.MatchString(req.Name) {
		return fmt.Errorf("invalid instance name")
	}

	// BIOS + disable_virtual_vga is unsupported — see the same guard in
	// LXDSetConfig for the full rationale. Fail before any `incus init`
	// runs so we don't leave a half-created VM behind.
	if req.DisableVirtualVGA && req.Firmware == "bios" {
		return fmt.Errorf("disable_virtual_vga is not supported on BIOS (CSM) VMs: SeaBIOS needs a virtual VGA for boot output and Intel iGPUs have no standalone VBIOS option ROM. Either keep the virtual VGA on (the iGPU is still passed through and Plex still gets exclusive access via i915) or switch this VM to UEFI firmware")
	}

	log := func(msg string) {
		if logCh != nil {
			logCh <- msg
		}
	}

	profile := req.Profile
	if profile == "" {
		profile = "default"
	}

	var args []string
	if req.Image == "" || req.Image == "__empty__" {
		// Create a VM with no OS image — user will boot from ISO or install later.
		args = []string{"init", "--empty", req.Name, "--vm", "-p", profile}
	} else {
		image := req.Image
		if !strings.Contains(image, ":") {
			image = "images:" + image
		}
		args = []string{"init", image, req.Name, "--vm", "-p", profile}
	}
	if req.VCPU > 0 {
		args = append(args, "-c", fmt.Sprintf("limits.cpu=%d", req.VCPU))
	}
	if req.MemoryMB > 0 {
		args = append(args, "-c", fmt.Sprintf("limits.memory=%dMiB", req.MemoryMB))
	}
	if req.MemoryHugepages {
		args = append(args, "-c", "limits.memory.hugepages=true")
	}
	// migration.stateful is set AFTER extra disks are attached (below) because LXD
	// rejects adding non-shared-pool disks while migration.stateful=true is already set.
	if req.AutoStart {
		args = append(args, "-c", "boot.autostart=true")
	}
	if req.ForceRunning {
		args = append(args, "-c", "user.zfsnas.force_running=true")
	}
	if req.CloudInit != "" {
		args = append(args, "-c", "user.user-data="+req.CloudInit)
	}
	if req.Firmware == "bios" {
		args = append(args, "-c", "security.csm=true")
	}
	// Root disk: pass pool/size/io.bus inline to "lxc init" via --device.
	// A previous "lxc config device override" pass after init didn't work
	// because LXD does NOT re-create or grow the underlying zvol when a
	// root disk's "size" key changes after the instance has been
	// initialised — it only updates the config record. Result: every VM
	// ended up with the LXD default 10 GB volume regardless of the
	// requested size. Setting size here ensures the volume is created
	// at the right size on the very first call.
	if req.RootPool != "" {
		args = append(args, "-d", "root,pool="+req.RootPool)
	}
	if req.RootSizeGB > 0 {
		// ZFS requires volsize to be a multiple of the pool's volblocksize
		// (16K by default). LXD prepends a 6144-byte image header to VM
		// block volumes, so we need (userBytes + 6144) to be 16K-aligned —
		// otherwise `zfs set volsize=…` fails with "must be a multiple of
		// volume block size (16K)" (e.g. 20GB → volsize=20000006144,
		// remainder 8192). Round up so the user always gets at least the
		// requested size.
		const headerBytes int64 = 6144
		const blockSize int64 = 16384
		sizeBytes := int64(req.RootSizeGB * 1000 * 1000 * 1000)
		aligned := ((sizeBytes+headerBytes+blockSize-1)/blockSize)*blockSize - headerBytes
		args = append(args, "-d", fmt.Sprintf("root,size=%dB", aligned))
	}
	if req.DiskBus != "" {
		args = append(args, "-d", "root,io.bus="+req.DiskBus)
	}
	log("Initialising VM " + req.Name + "…")
	if out, err := exec.Command("incus", args...).CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		// Translate the raw SQLite UNIQUE constraint error Incus surfaces
		// when an instance name is already taken into something a user
		// can read. Same idea applies to the project_id column variant.
		if strings.Contains(msg, "UNIQUE constraint failed") && strings.Contains(msg, "instances.name") {
			return fmt.Errorf("an instance named %q already exists", req.Name)
		}
		return fmt.Errorf("lxc init: %s: %w", msg, err)
	}
	// Apply secure boot setting post-init so we can fall back to raw.qemu on LXD 6.x
	// (which removed the security.secureboot config key).
	if req.Firmware == "bios" {
		// BIOS: disable secureboot; ignore error if key not supported by this LXD version.
		if out, err := exec.Command("incus", "config", "set", req.Name, "security.secureboot=false").CombinedOutput(); err != nil {
			if !strings.Contains(strings.TrimSpace(string(out)), "isn't supported") {
				log("WARNING: could not set security.secureboot=false: " + strings.TrimSpace(string(out)))
			}
		}
	} else if !req.SecureBoot {
		// UEFI without Secure Boot: try the key first, fall back to raw.qemu pflash flag.
		out, err := exec.Command("incus", "config", "set", req.Name, "security.secureboot=false").CombinedOutput()
		if err != nil && strings.Contains(strings.TrimSpace(string(out)), "isn't supported") {
			if err2 := lxdSetSecureBootRawQEMU(req.Name, false); err2 != nil {
				log("WARNING: secure boot (raw.qemu fallback): " + err2.Error())
			}
		}
	}
	// LXD sets a 100 MiB quota on the VM state dataset by default, which is too small
	// for stateful snapshots. Raise it to RAM + 20% now while the VM is stopped.
	if req.StatefulSnapshots {
		lxdEnsureStateQuota(req.Name)
	}

	// agent:config disk — Incus' magic device that exposes the in-VM agent
	// + cloud-init seed as a CD-ROM. RHEL-family images (AlmaLinux, Rocky,
	// CentOS Stream) declare image.requirements.cdrom_agent=true and refuse
	// to start without one ("This virtual machine image requires an
	// agent:config disk be added"). Other image families don't strictly
	// need it, but having it attached is harmless. Always add; if it's
	// already there from `incus init`, the call exits non-zero with
	// "device already exists" which we treat as a success no-op.
	if out, err := exec.Command("incus", "config", "device", "add", req.Name, "agent", "disk", "source=agent:config").CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if !strings.Contains(msg, "already exists") {
			log("WARNING: could not add agent:config disk: " + msg)
		}
	}

	// TPM device — add after init.
	if req.TPM {
		if out, err := exec.Command("incus", "config", "device", "add", req.Name, "tpm", "tpm").CombinedOutput(); err != nil {
			log("WARNING: could not add TPM device: " + strings.TrimSpace(string(out)))
		}
	}

	// Machine type — Incus 6.0.x lacks the qemu.machine.type config key
	// ("Unknown configuration key"); use raw.qemu's -machine flag instead.
	// Try the native key first so newer Incus versions get a clean config,
	// fall back to raw.qemu when Incus rejects it.
	if req.MachineType != "" {
		if out, err := exec.Command("incus", "config", "set", req.Name, "qemu.machine.type", req.MachineType).CombinedOutput(); err != nil {
			rawOut, _ := exec.Command("incus", "config", "get", req.Name, "raw.qemu").Output()
			lxdPatchConfig(req.Name, "raw.qemu",
				updateRawQEMUMachine(strings.TrimSpace(string(rawOut)), req.MachineType))
			log("info: machine type applied via raw.qemu (Incus rejected qemu.machine.type: " + strings.TrimSpace(string(out)) + ")")
		}
	}

	// SMBIOS types 1, 2, and 4 — written through raw.qemu's -smbios clauses.
	// nil structs are no-ops (no clause emitted). Each type is independent
	// so a VM can set type=1 only, or type=4 only, etc.
	if req.SMBIOS != nil || req.SMBIOSType2 != nil || req.SMBIOSType4 != nil {
		rawOut, _ := exec.Command("incus", "config", "get", req.Name, "raw.qemu").Output()
		current := strings.TrimSpace(string(rawOut))
		updated := updateRawQEMUSMBIOSType1(current, req.SMBIOS)
		updated = updateRawQEMUSMBIOSType2(updated, req.SMBIOSType2)
		updated = updateRawQEMUSMBIOSType4(updated, req.SMBIOSType4)
		if updated != current {
			lxdPatchConfig(req.Name, "raw.qemu", updated)
			log("info: SMBIOS fields applied via raw.qemu")
		}
	}

	// Set description (display name).
	if req.Description != "" {
		descJSON, _ := json.Marshal(req.Description)
		exec.Command("incus", "query", "-X", "PATCH", "/1.0/instances/"+req.Name,
			"--data", fmt.Sprintf(`{"description":%s}`, descJSON)).Run()
	}

	// (Root disk pool/size/io.bus are now applied inline at "lxc init" time
	// via --device flags above — see RootPool / RootSizeGB / DiskBus.
	// Doing it post-init didn't grow the underlying zvol on LXD 5.0.x.)

	// Add extra disks.
	for i, disk := range req.ExtraDisks {
		devName := disk.DeviceName
		if devName == "" {
			devName = fmt.Sprintf("disk%d", i+1)
		}
		// LXD 5.x: create a named block volume first, then attach with source=.
		volName := req.Name + "-" + devName
		volArgs := []string{"storage", "volume", "create", disk.Pool, volName,
			"--type", "block", fmt.Sprintf("size=%vGB", disk.SizeGB)}
		log("Adding disk " + devName + "…")
		if out, err := exec.Command("incus", volArgs...).CombinedOutput(); err != nil {
			log("WARNING: create volume for " + devName + ": " + strings.TrimSpace(string(out)))
			continue
		}
		dArgs := []string{"config", "device", "add", req.Name, devName, "disk",
			"pool=" + disk.Pool, "source=" + volName}
		if req.DiskBus != "" {
			dArgs = append(dArgs, "io.bus="+req.DiskBus)
		}
		if out, err := exec.Command("incus", dArgs...).CombinedOutput(); err != nil {
			log("WARNING: add disk " + devName + ": " + strings.TrimSpace(string(out)))
			exec.Command("incus", "storage", "volume", "delete", disk.Pool, volName).Run()
			continue
		}
		// Tag the volume so the snapshot/restore path knows whether to
		// take a coordinated volume snapshot every time the instance is
		// snapshotted. Default true so users who don't toggle anything
		// get the intuitive "snapshots cover the whole instance" behaviour.
		snap := "true"
		if !disk.IncludeInSnapshots {
			snap = "false"
		}
		if out, err := exec.Command("incus", "storage", "volume", "set",
			disk.Pool, volName, LXDSnapWithInstanceProperty, snap).CombinedOutput(); err != nil {
			log("Warning: tag " + LXDSnapWithInstanceProperty + " on " + volName + ": " + strings.TrimSpace(string(out)))
		}
		if disk.ReservePct > 0 {
			zfsPath := lxdFindZFSVol(volName)
			lxdSetZvolReservation(zfsPath, disk.ReservePct)
		}
	}

	// Attach existing ZVols / LXD-managed volumes.
	for i, disk := range req.ExistingDisks {
		devName := disk.DeviceName
		if devName == "" {
			devName = fmt.Sprintf("disk%d", len(req.ExtraDisks)+i+1)
		}
		log("Attaching existing disk " + devName + "…")
		var dArgs []string
		if pool, vol, ok := parseLXDVolRef(disk.DevPath); ok {
			dArgs = []string{"config", "device", "add", req.Name, devName, "disk",
				"pool=" + pool, "source=" + vol}
		} else if filepath.IsAbs(disk.DevPath) {
			dArgs = []string{"config", "device", "add", req.Name, devName, "disk",
				"source=" + disk.DevPath}
		} else {
			log("WARNING: existing disk " + devName + ": invalid dev_path, skipping")
			continue
		}
		if out, err := exec.Command("incus", dArgs...).CombinedOutput(); err != nil {
			log("WARNING: attach existing disk " + devName + ": " + strings.TrimSpace(string(out)))
		}
	}

	// Set migration.stateful now that all disks are attached. Setting it during
	// lxc init causes LXD to reject any subsequent disk-add for non-shared pools.
	//
	// Three exclusion conditions, all enforced by Incus itself:
	//
	//   1. TPM + migration.stateful=true is mutually exclusive.
	//   2. Additional disks from a non-shared pool + migration.stateful=true
	//      blocks VM start ("Only additional disks coming from a shared
	//      storage pool are supported with migration.stateful=true"). ZFS is
	//      local, so any ExtraDisk / ExistingDisk on a ZFS-backed host trips
	//      this — we keep the disks and skip the stateful flip.
	//   3. CDROMs (file-source readonly disk devices) + migration.stateful=true
	//      raises "Only Incus-managed disks are allowed with
	//      migration.stateful=true" — Incus refuses to add the CDROM. Setting
	//      stateful AFTER the CDROM also fails. We need to keep the CDROM
	//      with `boot.priority=10` so OVMF boots straight from it (the
	//      canonical Incus install path) — sacrificing stateful snapshots
	//      during install is the correct trade-off. Once the install ISO is
	//      removed (Eject), the user can re-enable Stateful Snapshots from
	//      the Edit dialog and migration.stateful=true will be applied.
	hasExtraDisks := len(req.ExtraDisks) > 0 || len(req.ExistingDisks) > 0
	hasCDROMsForStateful := false
	for _, p := range req.CDROMs {
		if p != "" && filepath.IsAbs(p) {
			hasCDROMsForStateful = true
			break
		}
	}
	if !hasCDROMsForStateful && req.CDROMPath != "" && filepath.IsAbs(req.CDROMPath) {
		hasCDROMsForStateful = true
	}
	if req.StatefulSnapshots && !req.TPM && !hasExtraDisks && !hasCDROMsForStateful {
		if out, err := exec.Command("incus", "config", "set", req.Name, "migration.stateful=true").CombinedOutput(); err != nil {
			log("WARNING: set migration.stateful: " + strings.TrimSpace(string(out)))
		}
	} else if req.StatefulSnapshots && hasExtraDisks {
		log("Note: stateful snapshots disabled — Incus only allows migration.stateful=true on VMs whose additional disks live on a shared storage pool. ZFS is local, so attaching extra disks rules this out. Remove the additional disks if you need stateful snapshots.")
	} else if req.StatefulSnapshots && req.TPM {
		log("Note: stateful snapshots disabled — TPM is mutually exclusive with migration.stateful=true.")
	} else if req.StatefulSnapshots && hasCDROMsForStateful {
		log("Note: stateful snapshots deferred while the install CDROM is attached — Incus rejects file-source CDROMs whenever migration.stateful=true. After the OS is installed and you eject the CDROM, re-enable Stateful Snapshots from the VM Edit dialog and the flag will be applied.")
	}

	// Add NICs.
	for i, nic := range req.NICs {
		devName := nic.DeviceName
		if devName == "" {
			devName = fmt.Sprintf("eth%d", i)
		}
		nArgs := []string{"config", "device", "add", req.Name, devName, "nic",
			"nictype=bridged", "parent=" + nic.Network,
		}
		if nic.MAC != "" {
			nArgs = append(nArgs, "hwaddr="+nic.MAC)
		}
		if nic.VlanID > 0 {
			nArgs = append(nArgs, fmt.Sprintf("vlan=%d", nic.VlanID))
		}
		log("Adding NIC " + devName + "…")
		if out, err := exec.Command("incus", nArgs...).CombinedOutput(); err != nil {
			log("WARNING: add NIC " + devName + ": " + strings.TrimSpace(string(out)))
		}
	}

	// Add USB devices.
	for _, usb := range req.USBDevices {
		if !usbIDRe.MatchString(usb.VendorID) || !usbIDRe.MatchString(usb.ProductID) {
			log("WARNING: skipping USB device with invalid IDs")
			continue
		}
		uArgs := []string{"config", "device", "add", req.Name, usb.DeviceName, "usb",
			"vendorid=" + usb.VendorID, "productid=" + usb.ProductID,
		}
		log("Adding USB device " + usb.DeviceName + "…")
		if out, err := exec.Command("incus", uArgs...).CombinedOutput(); err != nil {
			log("WARNING: add USB " + usb.DeviceName + ": " + strings.TrimSpace(string(out)))
		}
	}

	// Add PCI devices.
	for _, pci := range req.PCIDevices {
		if !pciAddrRe.MatchString(pci.Address) {
			log("WARNING: skipping PCI device with invalid address: " + pci.Address)
			continue
		}
		pArgs := []string{"config", "device", "add", req.Name, pci.DeviceName, "pci",
			"address=" + normPCIAddr(pci.Address),
		}
		log("Adding PCI device " + pci.DeviceName + "…")
		if out, err := exec.Command("incus", pArgs...).CombinedOutput(); err != nil {
			log("WARNING: add PCI " + pci.DeviceName + ": " + strings.TrimSpace(string(out)))
		}
	}

	// Apply rombar/x-vga/aer via raw.qemu before starting.
	applyPCIRawQEMU(req.Name, req.PCIDevices)

	// Apply "Disable virtual VGA adapter" via raw.qemu.conf override +
	// the disableVGAUserKey state flag (see disableVGAOverrideBody for
	// why state is tracked in a user key rather than via raw.qemu.conf
	// comment markers). Fresh VM → raw.qemu.conf is empty, so we just
	// write the override body when checked and the user key flips to
	// true; nothing to do when unchecked.
	if req.DisableVirtualVGA {
		val := applyDisableVirtualVGA("", true)
		lxdPatchConfig(req.Name, "raw.qemu.conf", val)
		exec.Command("incus", "config", "set", req.Name, disableVGAUserKey+"=true").Run() //nolint:errcheck
	}

	// Apply CPU pinning (overrides vCPU count when set).
	if req.CPUPin != "" {
		exec.Command("incus", "config", "set", req.Name, "limits.cpu", normalizeCPUPin(req.CPUPin)).Run()
	}

	// Apply socket topology via raw.qemu.
	if req.CPUSockets > 0 {
		out, _ := exec.Command("incus", "config", "get", req.Name, "raw.qemu").Output()
		cur := strings.TrimSpace(string(out))
		next := updateRawQEMUSockets(cur, req.CPUSockets)
		if next != cur {
			// key=value single-arg form, see lxdPatchConfig — values that
			// start with "-" (here "-smp sockets=N") would otherwise be
			// parsed by `incus` as an unknown shorthand flag.
			lxdPatchConfig(req.Name, "raw.qemu", next)
		}
	}

	// Attach CD/DVD drives via raw.qemu as SATA (Q35 ICH9 AHCI). Same
	// path as the edit flow, so create + edit behave identically. See
	// vmApplyCDROMs for the full rationale (Windows installers see SATA
	// natively; Incus-native disk devices auto-bind virtio-scsi which
	// requires a viostor side-load).
	paths := req.CDROMs
	if len(paths) == 0 && req.CDROMPath != "" {
		paths = []string{req.CDROMPath}
	}
	hasCDROMs := false
	for _, p := range paths {
		if p != "" && filepath.IsAbs(p) {
			hasCDROMs = true
			break
		}
	}
	if hasCDROMs {
		log("Attaching CD/DVD drives…")
		applyConf := func(k, v string) error {
			if v == "" {
				return exec.Command("incus", "config", "unset", req.Name, k).Run()
			}
			lxdPatchConfig(req.Name, k, v)
			return nil
		}
		vmApplyCDROMs(req.Name, paths, applyConf)
		// Surface what was applied so the user can verify on the live host
		// (Settings → Audit log + the create-job log). Without this, the
		// "no OS to boot from" symptom from a stale -boot strict=on was
		// invisible — the only way to diagnose it was `incus config show`
		// over SSH. Reading these two keys back also confirms that the
		// running ZNAS binary actually contains the boot-strict-off fix
		// (a stale binary writes the old raw.qemu and the user gets the
		// same failure on retry).
		log(fmt.Sprintf("Attached %d CD/DVD drive(s) as SATA on Q35 ICH9 AHCI.", len(paths)))
		if rqOut, _ := exec.Command("incus", "config", "get", req.Name, "raw.qemu").Output(); len(rqOut) > 0 {
			log("raw.qemu = " + strings.TrimSpace(string(rqOut)))
		}
		if aaOut, _ := exec.Command("incus", "config", "get", req.Name, "raw.apparmor").Output(); len(aaOut) > 0 {
			log("raw.apparmor = " + strings.TrimSpace(string(aaOut)))
		}
		log("Boot behaviour to expect: OVMF will attempt the empty root disk (~1 s, fails), then PXEv4/PXEv6/HTTPv4/HTTPv6 against eth0 (~30 s each, all time out on a NAT'd host), then the empty virtio-scsi CDROM (~1 s, fails), then the SATA CDROM (success). Total wait on a fresh UEFI VM: roughly 2–3 minutes of silent black screen before GRUB appears. Incus auto-assigns bootindex 0/1/2 to the root disk, eth0 NIC, and agent disk respectively, so the SATA CDROM has to land at bootindex 10 to avoid a duplicate-bootindex VM-start error — and OVMF tries lower bootindex first. If you don't want to wait, open the VGA console and press F12 from the keyboard dropdown to pick the CDROM manually.")
	}

	if req.AutoStart {
		log("Starting VM…")
		if out, err := exec.Command("incus", "start", req.Name).CombinedOutput(); err != nil {
			return fmt.Errorf("lxc start: %s: %w", strings.TrimSpace(string(out)), err)
		}
	}

	log("Done.")
	return nil
}

// LXDCreateContainer creates a container according to the request, writing
// progress lines to logCh.
func LXDCreateContainer(req LXDCreateContainerRequest, logCh chan<- string) error {
	if !lxdNameRe.MatchString(req.Name) {
		return fmt.Errorf("invalid instance name")
	}

	log := func(msg string) {
		if logCh != nil {
			logCh <- msg
		}
	}

	profile := req.Profile
	if profile == "" {
		profile = "default"
	}

	image := req.Image
	if !strings.Contains(image, ":") {
		image = "images:" + image
	}

	args := []string{"init", image, req.Name, "-p", profile}
	if req.CPUCores > 0 {
		args = append(args, "-c", fmt.Sprintf("limits.cpu=%d", req.CPUCores))
	}
	if req.CPUShares > 0 && req.CPUShares <= 10 {
		args = append(args, "-c", fmt.Sprintf("limits.cpu.priority=%d", req.CPUShares))
	}
	if req.CPULimitPct > 0 && req.CPULimitPct <= 100 {
		args = append(args, "-c", fmt.Sprintf("limits.cpu.allowance=%dms/100ms", req.CPULimitPct))
	}
	if req.MemoryMB > 0 {
		args = append(args, "-c", fmt.Sprintf("limits.memory=%dMiB", req.MemoryMB))
	}
	// LXD's limits.memory.swap is a boolean (allow swapping or not), NOT a
	// size — there's no per-instance swap-size knob in LXD. Translate the
	// numeric SwapMB field accordingly: -1 => disable swap, 0 => leave at
	// the LXD default (allow), any positive value => allow (the size is not
	// honored by LXD). The UI's MB/GB unit selector is preserved for now to
	// keep the frontend stable, but only the sign matters here.
	if req.SwapMB == -1 {
		args = append(args, "-c", "limits.memory.swap=false")
	} else if req.SwapMB > 0 {
		args = append(args, "-c", "limits.memory.swap=true")
	}
	if req.AutoStart {
		args = append(args, "-c", "boot.autostart=true")
	}
	if req.ForceRunning {
		args = append(args, "-c", "user.zfsnas.force_running=true")
	}
	if req.StartupOrder > 0 {
		args = append(args, "-c", fmt.Sprintf("boot.autostart.priority=%d", req.StartupOrder))
	}
	if req.StartupDelay > 0 {
		args = append(args, "-c", fmt.Sprintf("boot.autostart.delay=%d", req.StartupDelay))
	}
	if !req.Unprivileged {
		args = append(args, "-c", "security.privileged=true")
	}
	if req.Nesting {
		args = append(args, "-c", "security.nesting=true")
	}
	if req.FeatureKeyctl {
		// security.syscalls.allow is a whitelist; setting it to "keyctl"
		// denies every other syscall and the container can't even boot
		// (seccomp SIGSYS on close()). Use the dedicated intercept key
		// when supported; on older LXD it isn't accepted and the default
		// seccomp profile already permits keyctl, so do nothing.
		if lxdSupportsConfigKey("security.syscalls.intercept.keyctl") {
			args = append(args, "-c", "security.syscalls.intercept.keyctl=true")
		}
	}

	log("Initialising container " + req.Name + "…")
	if out, err := exec.Command("incus", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("lxc init: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Set description (display name).
	if req.Description != "" {
		descJSON, _ := json.Marshal(req.Description)
		exec.Command("incus", "query", "-X", "PATCH", "/1.0/instances/"+req.Name,
			"--data", fmt.Sprintf(`{"description":%s}`, descJSON)).Run()
	}

	// Root disk pool + size.
	if req.DiskSizeGB > 0 || req.RootPool != "" {
		dArgs := []string{"config", "device", "override", req.Name, "root"}
		if req.RootPool != "" {
			dArgs = append(dArgs, "pool="+req.RootPool)
		}
		if req.DiskSizeGB > 0 {
			// %vGB so fractional GB (sub-GB sizes from a "MB" unit selector)
			// pass through as e.g. "0.1GB" — incus accepts decimals here.
			dArgs = append(dArgs, fmt.Sprintf("size=%vGB", req.DiskSizeGB))
		}
		log("Configuring root disk…")
		if out, err := exec.Command("incus", dArgs...).CombinedOutput(); err != nil {
			log("WARNING: root disk size: " + strings.TrimSpace(string(out)))
		}
	}

	// Add NICs.
	for i, nic := range req.NICs {
		devName := nic.DeviceName
		if devName == "" {
			devName = fmt.Sprintf("eth%d", i)
		}
		nArgs := []string{"config", "device", "add", req.Name, devName, "nic",
			"nictype=bridged", "parent=" + nic.Network,
		}
		if nic.MAC != "" {
			nArgs = append(nArgs, "hwaddr="+nic.MAC)
		}
		if nic.VlanID > 0 {
			nArgs = append(nArgs, fmt.Sprintf("vlan=%d", nic.VlanID))
		}
		// Static IP is configured via pre-start file push — bridged NICs
		// do not accept ipv4.address/ipv4.gateway at the device level.
		log("Adding NIC " + devName + "…")
		if out, err := exec.Command("incus", nArgs...).CombinedOutput(); err != nil {
			log("WARNING: add NIC " + devName + ": " + strings.TrimSpace(string(out)))
		}

		// Push static network config files to the stopped container so that
		// it boots with the correct IP immediately (no DHCP race).
		if nic.IPv4Mode == "static" && nic.IPv4Addr != "" {
			log("Pre-configuring static IP for " + devName + "…")
			_pushStaticNetworkConfig(req.Name, devName, nic)
		}
	}

	// Add passthrough devices.
	for _, dev := range req.Devices {
		dArgs := []string{"config", "device", "add", req.Name, dev.DeviceName, dev.Type}
		if dev.HostPath != "" {
			dArgs = append(dArgs, "path="+dev.HostPath)
		}
		for k, v := range dev.Extra {
			dArgs = append(dArgs, k+"="+v)
		}
		log("Adding device " + dev.DeviceName + "…")
		if out, err := exec.Command("incus", dArgs...).CombinedOutput(); err != nil {
			log("WARNING: add device " + dev.DeviceName + ": " + strings.TrimSpace(string(out)))
		}
	}

	// FUSE: expose /dev/fuse inside the container.
	if req.FeatureFUSE {
		log("Adding FUSE device…")
		if out, err := exec.Command("incus", "config", "device", "add", req.Name,
			"fuse", "unix-char", "path=/dev/fuse").CombinedOutput(); err != nil {
			log("WARNING: add fuse device: " + strings.TrimSpace(string(out)))
		}
	}

	// Extra (newly-created) disks: incus storage volumes mounted at /<name>.
	// Containers can't take "block"-type devices like VMs can; they get a
	// pool-managed filesystem volume instead. SizeGB allows fractional GB so
	// the UI's MB selector works (300MB → 0.3 GB).
	for i, disk := range req.ExtraDisks {
		devName := disk.DeviceName
		if devName == "" {
			devName = fmt.Sprintf("disk%d", i+1)
		}
		pool := disk.Pool
		if pool == "" {
			pool = req.RootPool
		}
		if pool == "" {
			// Fall back to the first storage pool incus knows about.
			if out, err := exec.Command("incus", "storage", "list", "--format", "csv").Output(); err == nil {
				for _, line := range strings.Split(string(out), "\n") {
					if line == "" {
						continue
					}
					if i := strings.IndexByte(line, ','); i > 0 {
						pool = line[:i]
						break
					}
				}
			}
		}
		if pool == "" {
			log("WARNING: no storage pool available — skipping extra disk " + devName)
			continue
		}
		volName := req.Name + "-" + devName
		// Storage volume `size` option doesn't accept fractional GB strings
		// like "0.3GB"; format as integer bytes (incus parses raw byte counts
		// without a unit suffix) so MB-precision sizes still work.
		sizeBytes := int64(disk.SizeGB * 1000 * 1000 * 1000)
		log(fmt.Sprintf("Creating storage volume %s/%s (size=%d bytes)…", pool, volName, sizeBytes))
		args := []string{"storage", "volume", "create", pool, volName}
		if sizeBytes > 0 {
			args = append(args, fmt.Sprintf("size=%d", sizeBytes))
		}
		if out, err := exec.Command("incus", args...).CombinedOutput(); err != nil {
			log("WARNING: create volume " + volName + ": " + strings.TrimSpace(string(out)))
			continue
		}
		log("Attaching " + devName + " → /" + devName + "…")
		if out, err := exec.Command("incus", "config", "device", "add", req.Name,
			devName, "disk", "pool="+pool, "source="+volName, "path=/"+devName).CombinedOutput(); err != nil {
			log("WARNING: attach volume " + volName + ": " + strings.TrimSpace(string(out)))
			continue
		}
		// Tag the volume so the snapshot/restore path knows whether to
		// also snapshot it alongside the container. Default true.
		snap := "true"
		if !disk.IncludeInSnapshots {
			snap = "false"
		}
		if out, err := exec.Command("incus", "storage", "volume", "set",
			pool, volName, LXDSnapWithInstanceProperty, snap).CombinedOutput(); err != nil {
			log("Warning: tag " + LXDSnapWithInstanceProperty + " on " + volName + ": " + strings.TrimSpace(string(out)))
		}
	}

	// Existing ZVols / pool volumes attached as-is (no creation step).
	for i, ed := range req.ExistingDisks {
		devName := ed.DeviceName
		if devName == "" {
			devName = fmt.Sprintf("disk%d", len(req.ExtraDisks)+i+1)
		}
		log("Attaching existing disk " + ed.DevPath + " as " + devName + "…")
		if out, err := exec.Command("incus", "config", "device", "add", req.Name,
			devName, "disk", "source="+ed.DevPath, "path=/"+devName).CombinedOutput(); err != nil {
			log("WARNING: attach existing disk " + devName + ": " + strings.TrimSpace(string(out)))
		}
	}

	needStart := req.AutoStart || req.RootPassword != "" || _hasStaticIPConfig(req.NICs)
	if needStart {
		log("Starting container…")
		if out, err := exec.Command("incus", "start", req.Name).CombinedOutput(); err != nil {
			return fmt.Errorf("lxc start: %s: %w", strings.TrimSpace(string(out)), err)
		}
		// Wait for the container's init system to finish starting up so that
		// exec commands (chpasswd, ip) land on a fully-initialized system.
		if err := _waitContainerReady(req.Name, 60); err != nil {
			log("WARNING: container readiness: " + err.Error())
		}
	}

	// Apply static IPs via ip commands for immediate effect in the current
	// boot session (the config files pushed before start handle reboots).
	for i, nic := range req.NICs {
		if nic.IPv4Mode != "static" || nic.IPv4Addr == "" {
			continue
		}
		devName := nic.DeviceName
		if devName == "" {
			devName = fmt.Sprintf("eth%d", i)
		}
		log("Applying static IP for " + devName + "…")
		_applyStaticIPCommands(req.Name, devName, nic)
	}

	// Set root password via chpasswd (requires running container).
	if req.RootPassword != "" {
		log("Setting root password…")
		cmd := exec.Command("incus", "exec", req.Name, "--", "chpasswd")
		cmd.Stdin = strings.NewReader("root:" + req.RootPassword + "\n")
		if out, err := cmd.CombinedOutput(); err != nil {
			log("WARNING: set root password: " + strings.TrimSpace(string(out)))
		}
	}

	// Write /etc/resolv.conf for static-IP NICs that have DNS configured.
	// Remove the file first — it is often a symlink whose target dir may not exist yet.
	if dnsLines := _collectDNSLines(req.NICs); len(dnsLines) > 0 {
		log("Configuring DNS…")
		resolvConf := strings.Join(dnsLines, "\n") + "\n"
		cmd := exec.Command("incus", "exec", req.Name, "--", "/bin/sh", "-c",
			"rm -f /etc/resolv.conf && cat > /etc/resolv.conf")
		cmd.Stdin = strings.NewReader(resolvConf)
		if out, err := cmd.CombinedOutput(); err != nil {
			log("WARNING: set DNS: " + strings.TrimSpace(string(out)))
		}
	}

	// Stop if we only started for post-init tasks and auto_start was not requested.
	if !req.AutoStart && needStart {
		log("Stopping container…")
		MarkUserInitiatedStop(req.Name)
		exec.Command("incus", "stop", req.Name).Run()
	}

	log("Done.")
	return nil
}

func _hasStaticIPConfig(nics []LXDNIC) bool {
	for _, n := range nics {
		if n.IPv4Mode == "static" {
			return true
		}
	}
	return false
}

// _waitContainerReady waits until the container's init system is fully up.
// It first polls for exec access, then for systemctl is-system-running.
func _waitContainerReady(name string, timeoutSec int) error {
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	// Phase 1: wait until exec is possible.
	for time.Now().Before(deadline) {
		if exec.Command("incus", "exec", name, "--", "true").Run() == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if time.Now().After(deadline) {
		return fmt.Errorf("exec not available after %d seconds", timeoutSec)
	}
	// Phase 2: wait for systemd to finish initialising (systemd-based containers).
	for time.Now().Before(deadline) {
		out, err := exec.Command("incus", "exec", name, "--", "systemctl", "is-system-running").Output()
		if err != nil {
			// systemctl not available (non-systemd); a brief extra wait is enough.
			time.Sleep(3 * time.Second)
			return nil
		}
		state := strings.TrimSpace(string(out))
		if state == "running" || state == "degraded" {
			return nil
		}
		time.Sleep(time.Second)
	}
	return fmt.Errorf("system not running after %d seconds", timeoutSec)
}

// _pushStaticNetworkConfig writes static IP config files to a stopped (or running)
// container for both systemd-networkd and ifupdown-based distros.
func _pushStaticNetworkConfig(ctName, devName string, nic LXDNIC) {
	// systemd-networkd config (Debian/Ubuntu default in LXD images).
	var nd strings.Builder
	nd.WriteString("[Match]\nName=" + devName + "\n\n[Network]\nDHCP=false\nAddress=" + nic.IPv4Addr + "\n")
	if nic.IPv4GW != "" {
		nd.WriteString("Gateway=" + nic.IPv4GW + "\n")
	}
	if nic.DNS1 != "" {
		nd.WriteString("DNS=" + nic.DNS1 + "\n")
	}
	if nic.DNS2 != "" {
		nd.WriteString("DNS=" + nic.DNS2 + "\n")
	}
	cmd := exec.Command("incus", "file", "push", "-", ctName+"/etc/systemd/network/"+devName+".network")
	cmd.Stdin = strings.NewReader(nd.String())
	cmd.Run()

	// ifupdown config (fallback for containers without systemd-networkd).
	var ifd strings.Builder
	ifd.WriteString("auto " + devName + "\n")
	ifd.WriteString("iface " + devName + " inet static\n")
	ifd.WriteString("    address " + nic.IPv4Addr + "\n")
	if nic.IPv4GW != "" {
		ifd.WriteString("    gateway " + nic.IPv4GW + "\n")
	}
	cmd2 := exec.Command("incus", "file", "push", "-", ctName+"/etc/network/interfaces.d/"+devName)
	cmd2.Stdin = strings.NewReader(ifd.String())
	cmd2.Run()
}

// _applyStaticIPCommands applies a static IP immediately via ip commands in a
// running container (the persistent config was already pushed before start).
func _applyStaticIPCommands(ctName, devName string, nic LXDNIC) {
	exec.Command("incus", "exec", ctName, "--", "ip", "link", "set", devName, "up").Run()
	exec.Command("incus", "exec", ctName, "--", "ip", "addr", "flush", "dev", devName).Run()
	exec.Command("incus", "exec", ctName, "--", "ip", "addr", "add", nic.IPv4Addr, "dev", devName).Run()
	if nic.IPv4GW != "" {
		exec.Command("incus", "exec", ctName, "--", "ip", "route", "replace", "default", "via", nic.IPv4GW).Run()
	}
}

func _collectDNSLines(nics []LXDNIC) []string {
	seen := map[string]bool{}
	var lines []string
	for _, n := range nics {
		if n.IPv4Mode != "static" {
			continue
		}
		for _, ip := range []string{n.DNS1, n.DNS2} {
			if ip != "" && !seen[ip] {
				seen[ip] = true
				lines = append(lines, "nameserver "+ip)
			}
		}
	}
	return lines
}

// ListUSBDevices parses `lsusb` output and returns host USB devices.
func ListUSBDevices() ([]USBDevice, error) {
	out, err := exec.Command("lsusb").Output()
	if err != nil {
		return nil, fmt.Errorf("lsusb: %w", err)
	}
	// Line format: Bus 001 Device 002: ID 8087:0024 Intel Corp. Integrated Rate Matching Hub
	re := regexp.MustCompile(`Bus (\d+) Device (\d+): ID ([0-9a-fA-F]{4}):([0-9a-fA-F]{4})\s+(.*)`)
	var devices []USBDevice
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		m := re.FindStringSubmatch(scanner.Text())
		if m == nil {
			continue
		}
		devices = append(devices, USBDevice{
			Bus:       m[1],
			Device:    m[2],
			VendorID:  m[3],
			ProductID: m[4],
			Desc:      strings.TrimSpace(m[5]),
		})
	}
	return devices, nil
}

// ListPCIDevices parses `lspci -vmm` output and returns host PCI devices.
func ListPCIDevices() ([]PCIDevice, error) {
	out, err := exec.Command("lspci", "-vmm").Output()
	if err != nil {
		return nil, fmt.Errorf("lspci: %w", err)
	}

	var devices []PCIDevice
	var cur PCIDevice
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if cur.Slot != "" {
				devices = append(devices, cur)
				cur = PCIDevice{}
			}
			continue
		}
		parts := strings.SplitN(line, ":\t", 2)
		if len(parts) != 2 {
			continue
		}
		key, val := parts[0], strings.TrimSpace(parts[1])
		switch key {
		case "Slot":
			cur.Slot = val
		case "Class":
			cur.Class = val
		case "Vendor":
			cur.Vendor = val
		case "Device":
			cur.Device = val
		}
	}
	if cur.Slot != "" {
		devices = append(devices, cur)
	}
	return devices, nil
}

// LXDSnapshot represents a single LXD instance snapshot.
type LXDSnapshot struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Stateful  bool      `json:"stateful"` // true when memory state was captured
}

// ListLXDSnapshots returns all snapshots for the named instance.
func ListLXDSnapshots(name string) ([]LXDSnapshot, error) {
	type rawSnap struct {
		Name      string    `json:"name"`
		CreatedAt time.Time `json:"created_at"`
		Stateful  bool      `json:"stateful"`
	}
	out, err := exec.Command("incus", "query", "/1.0/instances/"+name+"/snapshots?recursion=1").Output()
	if err != nil {
		return nil, fmt.Errorf("lxc query snapshots: %w", err)
	}
	var raws []rawSnap
	if err := json.Unmarshal(out, &raws); err != nil {
		return nil, fmt.Errorf("parse snapshots: %w", err)
	}
	snaps := make([]LXDSnapshot, len(raws))
	for i, r := range raws {
		snaps[i] = LXDSnapshot{
			Name:      r.Name,
			CreatedAt: r.CreatedAt,
			Stateful:  r.Stateful,
		}
	}
	return snaps, nil
}

// lxdVMRamBytes returns the VM's configured RAM in bytes (4 GiB fallback).
func lxdVMRamBytes(name string) int64 {
	if out, e := exec.Command("incus", "config", "get", name, "limits.memory").Output(); e == nil {
		mem := strings.TrimSpace(string(out))
		var mult int64 = 1
		if strings.HasSuffix(mem, "GB") {
			mult = 1 << 30
			mem = strings.TrimSuffix(mem, "GB")
		} else if strings.HasSuffix(mem, "MB") {
			mult = 1 << 20
			mem = strings.TrimSuffix(mem, "MB")
		}
		if v, e2 := strconv.ParseInt(strings.TrimSpace(mem), 10, 64); e2 == nil && v > 0 {
			return v * mult
		}
	}
	return 4 << 30 // fallback: 4 GiB
}

// lxdStateDataset returns the ZFS dataset path for the VM's state directory
// (e.g. "tank/lxd-base/virtual-machines/<name>").
func lxdStateDataset(name string) string {
	// Find root pool from LXD storage pool config.
	poolName := ""
	if out, e := exec.Command("incus", "config", "device", "show", name).Output(); e == nil {
		for _, line := range strings.Split(string(out), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "pool:") {
				poolName = strings.TrimSpace(strings.TrimPrefix(line, "pool:"))
			}
		}
	}
	if poolName == "" {
		return ""
	}
	if out, e := exec.Command("incus", "query", "/1.0/storage-pools/"+poolName).Output(); e == nil {
		var info struct {
			Config map[string]string `json:"config"`
		}
		if json.Unmarshal(out, &info) == nil {
			if src := info.Config["source"]; src != "" {
				return src + "/virtual-machines/" + name
			}
		}
	}
	return ""
}

// lxdEnsureStateQuota raises both the LXD size.state device config and the
// underlying ZFS quota to at least RAM + 20% so stateful snapshots can write
// the full memory image. Both must be set: LXD enforces size.state at start-up
// ("Stateful start requires limits.memory < size.state"), and ZFS needs the
// quota raised from LXD's default 100 MiB. Best-effort; errors are ignored.
func lxdEnsureStateQuota(name string) {
	ramBytes := lxdVMRamBytes(name)
	// Quota = RAM + 20% overhead, rounded up to the nearest GiB, minimum 2 GiB.
	quotaBytes := ramBytes + ramBytes/5
	const gib = int64(1 << 30)
	quotaGiB := (quotaBytes + gib - 1) / gib
	if quotaGiB < 2 {
		quotaGiB = 2
	}
	sizeVal := fmt.Sprintf("%dGiB", quotaGiB)

	// Tell LXD the size.state so it allows stateful start/migration.
	// If root is inherited from a profile, "set" fails — use "override" instead.
	if out, err := exec.Command("incus", "config", "device", "set", name, "root", "size.state="+sizeVal).CombinedOutput(); err != nil {
		if strings.Contains(string(out), "profile") {
			exec.Command("incus", "config", "device", "override", name, "root", "size.state="+sizeVal).Run() //nolint:errcheck
		}
	}

	// Also raise the ZFS quota on the state dataset (LXD defaults to 100 MiB).
	ds := lxdStateDataset(name)
	if ds == "" {
		return
	}
	if out, e := exec.Command("zfs", "get", "-Hp", "-o", "value", "quota", ds).Output(); e == nil {
		if cur, e2 := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); e2 == nil && cur >= quotaGiB*gib {
			return
		}
	}
	exec.Command("sudo", "zfs", "set", fmt.Sprintf("quota=%dG", quotaGiB), ds).Run() //nolint:errcheck
}

// lxdStateAvail returns (availBytes, neededBytes, error) for the LXD state filesystem
// of the named VM. neededBytes is the VM's configured RAM.
func lxdStateAvail(name string) (avail, needed int64, err error) {
	needed = lxdVMRamBytes(name)
	ds := lxdStateDataset(name)
	if ds == "" {
		return 0, needed, fmt.Errorf("dataset not found")
	}
	if out, e := exec.Command("zfs", "get", "-Hp", "-o", "value", "available", ds).Output(); e == nil {
		if v, e2 := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); e2 == nil {
			avail = v
		}
	}
	if avail == 0 {
		return 0, needed, fmt.Errorf("could not read available bytes")
	}
	return avail, needed, nil
}

// CreateLXDSnapshot creates a snapshot of the named instance.
// snapName may be empty (LXD will auto-name it). stateful=true captures memory;
// for VMs this requires migration.stateful=true which is set automatically.
func CreateLXDSnapshot(name, snapName string, stateful bool) error {
	if stateful {
		// Check whether migration.stateful is already enabled.
		out, _ := exec.Command("incus", "config", "get", name, "migration.stateful").Output()
		if strings.TrimSpace(string(out)) != "true" {
			// Try to set it. This fails when the VM is running — LXD requires the
			// VM to be stopped to change this key because it changes QEMU init args.
			if setOut, err := exec.Command("incus", "config", "set", name, "migration.stateful", "true").CombinedOutput(); err != nil {
				msg := strings.TrimSpace(string(setOut))
				if strings.Contains(msg, "running") || strings.Contains(msg, "cannot be updated") {
					return fmt.Errorf(
						"stateful snapshots require migration.stateful=true, which can only be set while the VM is stopped. " +
							"Stop the VM, then enable \"Stateful Snapshots\" in Edit, start it again, and retry.")
				}
				return fmt.Errorf("set migration.stateful: %s", msg)
			}
		}
	}
	// Before attempting a stateful snapshot, ensure the LXD state dataset has enough
	// quota for the memory image. LXD hard-codes the initial quota at 100 MiB which is
	// far too small; lxdEnsureStateQuota raises it to RAM + 20% if needed.
	if stateful {
		lxdEnsureStateQuota(name)
		if avail, needed, err := lxdStateAvail(name); err == nil && avail < needed {
			return fmt.Errorf(
				"not enough space in VM state filesystem for stateful snapshot: %dMB available, ~%dMB needed (VM RAM). "+
					"Stop the VM to allow LXD to release any leftover state files, then retry.",
				avail>>20, needed>>20)
		}
	}

	// Newer Incus (≥ 6.x as shipped in Ubuntu 26.04) removed the
	// `incus snapshot <inst> [<name>]` short form — `incus snapshot` now
	// requires an explicit subcommand (create/delete/restore/list/show).
	// The `snapshot create` form has been valid for a long time, so this
	// also works on older Incus.
	args := []string{"snapshot", "create", name}
	if snapName != "" {
		args = append(args, snapName)
	}
	if stateful {
		args = append(args, "--stateful")
	}
	// Stateful snapshots save live VM memory to disk and can take minutes for large VMs.
	// Use a 3-minute timeout so a hung QEMU save doesn't block the HTTP handler forever.
	timeout := 30 * time.Second
	if stateful {
		timeout = 3 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "incus", args...).CombinedOutput(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("snapshot timed out after %s. Common causes: (1) VM dataset quota is full — QEMU must write the full memory state (~RAM size) to disk; increase the dataset quota or free space. (2) migration.stateful not set before last VM start — stop the VM, enable 'Stateful Snapshots' in Edit, start again, then retry.", timeout)
		}
		msg := strings.TrimSpace(string(out))
		if strings.Contains(msg, "USB devices cannot be used when migration.stateful is enabled") {
			return fmt.Errorf("stateful snapshots are incompatible with USB passthrough devices. Remove USB passthrough devices in Edit, or take a non-stateful snapshot instead.")
		}
		if strings.Contains(msg, "Monitor is disconnected") {
			return fmt.Errorf("stateful snapshot failed: QEMU monitor disconnected. Ensure the VM started after enabling 'Stateful Snapshots' in Edit (stop → enable → start → snapshot).")
		}
		return fmt.Errorf("%s", msg)
	}
	// Coordinated volume snapshot: tagged custom volumes get a matching
	// snapshot so a later restore can roll their data back too.
	snapshotTaggedVolumes(name, snapName)
	return nil
}

// RestoreLXDSnapshot reverts the instance to the named snapshot.
// When removeSubsequent is true, all snapshots created after snapName are
// deleted first — required by the ZFS storage backend when the target is not
// the most recent snapshot.
func RestoreLXDSnapshot(name, snapName string, removeSubsequent bool) error {
	if removeSubsequent {
		snaps, err := ListLXDSnapshots(name)
		if err != nil {
			return fmt.Errorf("list snapshots: %w", err)
		}
		sort.Slice(snaps, func(i, j int) bool {
			return snaps[i].CreatedAt.Before(snaps[j].CreatedAt)
		})
		targetIdx := -1
		for i, s := range snaps {
			if s.Name == snapName {
				targetIdx = i
				break
			}
		}
		if targetIdx >= 0 {
			for i := len(snaps) - 1; i > targetIdx; i-- {
				if err := DeleteLXDSnapshot(name, snaps[i].Name); err != nil {
					return fmt.Errorf("delete subsequent snapshot %q: %w", snaps[i].Name, err)
				}
			}
		}
	}
	if out, err := exec.Command("incus", "snapshot", "restore", name, snapName).CombinedOutput(); err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	// Roll attached tagged volumes back to their matching snapshot so the
	// data disks line up with the instance's just-restored state.
	restoreTaggedVolumes(name, snapName)
	return nil
}

// CloneLXDFromSnapshot creates a new instance by copying from source/snapshot.
// If targetPool is non-empty, the clone's root disk is placed in that
// storage pool (`incus copy -s <pool>`); otherwise the source pool is reused.
// The description is applied via a PATCH to the LXD API after creation.
func CloneLXDFromSnapshot(sourceName, snapName, newName, description, targetPool string) error {
	src := sourceName + "/" + snapName
	args := []string{"copy"}
	if targetPool != "" {
		args = append(args, "-s", targetPool)
	}
	args = append(args, src, newName)
	if out, err := exec.Command("incus", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	if description != "" {
		body, _ := json.Marshal(map[string]string{"description": description})
		// Ignore errors — the clone already exists; description is cosmetic.
		exec.Command("incus", "query", "-X", "PATCH",
			"/1.0/instances/"+newName, "-d", string(body)).Run()
	}
	return nil
}

// CloneLXDInstance copies an instance directly (no snapshot needed).
// If targetPool is non-empty, the clone's root disk is placed in that
// storage pool (`incus copy -s <pool>`); otherwise the source pool is reused.
// The source may be running; LXD will take a live copy.
func CloneLXDInstance(sourceName, newName, description, targetPool string) error {
	args := []string{"copy"}
	if targetPool != "" {
		args = append(args, "-s", targetPool)
	}
	args = append(args, sourceName, newName)
	if out, err := exec.Command("incus", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	if description != "" {
		body, _ := json.Marshal(map[string]string{"description": description})
		exec.Command("incus", "query", "-X", "PATCH",
			"/1.0/instances/"+newName, "-d", string(body)).Run()
	}
	return nil
}

// DeleteLXDSnapshot deletes a single snapshot from the instance.
func DeleteLXDSnapshot(name, snapName string) error {
	if out, err := exec.Command("incus", "snapshot", "delete", name, snapName).CombinedOutput(); err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	// Clean up the matching volume snapshots so we don't leak storage.
	deleteTaggedVolumeSnapshots(name, snapName)
	return nil
}

// lxdSnapTaggedVolumes returns the (pool, volume) pairs of every disk
// device on `instance` whose source custom volume carries
// `user.znas:snap_with_instance=true`. Used by the snapshot/restore/delete
// path to keep volume snapshots in lockstep with the instance's own.
//
// We deliberately read the LIVE instance config rather than a cached copy
// so the lookup picks up disks that were added through the Edit modal
// after creation, including ones whose tag was flipped via direct
// `incus storage volume set …` outside ZNAS.
func lxdSnapTaggedVolumes(instance string) [][2]string {
	out, err := exec.Command("incus", "config", "show", instance, "--expanded").Output()
	if err != nil {
		return nil
	}
	// Cheap textual scan rather than a full YAML parse — the relevant
	// region looks like:
	//   data1:
	//     pool: tank
	//     source: u2604-2-data1
	//     type: disk
	// We collect (pool, source) per device that has type=disk.
	type devEntry struct{ pool, source string }
	devs := map[string]*devEntry{}
	currentDev := ""
	insideDevices := false
	for _, line := range strings.Split(string(out), "\n") {
		if line == "devices:" {
			insideDevices = true
			continue
		}
		if insideDevices && len(line) > 0 && line[0] != ' ' && line[0] != '\t' {
			break
		}
		if !insideDevices {
			continue
		}
		// "  data1:" — two-space indent device key
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "    ") && strings.HasSuffix(strings.TrimSpace(line), ":") {
			currentDev = strings.TrimSuffix(strings.TrimSpace(line), ":")
			devs[currentDev] = &devEntry{}
			continue
		}
		if currentDev == "" {
			continue
		}
		trim := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trim, "pool: "):
			devs[currentDev].pool = strings.TrimSpace(strings.TrimPrefix(trim, "pool:"))
		case strings.HasPrefix(trim, "source: "):
			devs[currentDev].source = strings.TrimSpace(strings.TrimPrefix(trim, "source:"))
		case trim == "type: disk":
			// keep
		default:
			// no-op; we don't care about other fields
		}
	}
	var out2 [][2]string
	for _, d := range devs {
		if d.pool == "" || d.source == "" {
			continue
		}
		// Probe the volume's user property. `incus storage volume get`
		// prints the value (or empty) and exits 0; sudo not needed for
		// `get`. The volume is custom-type — instance-scoped volumes
		// (which are already part of incus snapshot) don't carry the tag.
		val, err := exec.Command("incus", "storage", "volume", "get",
			d.pool, d.source, LXDSnapWithInstanceProperty).Output()
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(val)) == "true" {
			out2 = append(out2, [2]string{d.pool, d.source})
		}
	}
	return out2
}

// snapshotTaggedVolumes / restoreTaggedVolumes / deleteTaggedVolumeSnapshots
// fan a single name out across every Snap-tagged custom volume the instance
// references. Errors are downgraded to log lines because the instance-level
// op already succeeded and rolling that back would be more disruptive than
// the inconsistency we'd be guarding against.
func snapshotTaggedVolumes(instance, snapName string) {
	for _, pv := range lxdSnapTaggedVolumes(instance) {
		args := []string{"storage", "volume", "snapshot", "create", pv[0], pv[1], snapName}
		if out, err := exec.Command("incus", args...).CombinedOutput(); err != nil {
			// Older Incus uses `storage volume snapshot <pool> <vol> <snap>` (no "create")
			args2 := []string{"storage", "volume", "snapshot", pv[0], pv[1], snapName}
			if out2, err2 := exec.Command("incus", args2...).CombinedOutput(); err2 != nil {
				_ = out
				_ = out2
				continue
			}
		}
	}
}

func restoreTaggedVolumes(instance, snapName string) {
	for _, pv := range lxdSnapTaggedVolumes(instance) {
		exec.Command("incus", "storage", "volume", "restore", pv[0], pv[1], snapName).Run() //nolint:errcheck
	}
}

func deleteTaggedVolumeSnapshots(instance, snapName string) {
	for _, pv := range lxdSnapTaggedVolumes(instance) {
		// New form first, then old.
		if err := exec.Command("incus", "storage", "volume", "snapshot", "delete", pv[0], pv[1], snapName).Run(); err == nil {
			continue
		}
		exec.Command("incus", "storage", "volume", "delete", pv[0], pv[1]+"/"+snapName).Run() //nolint:errcheck
	}
}

// LXDLogEntry is a single parsed line from an instance log file.
type LXDLogEntry struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

// GetLXDInstanceLogs returns recent log entries for the named instance.
// It reads the lxc.log file via the LXD API (lxc query) and falls back
// to lxc info --show-log when no structured log is available.
func GetLXDInstanceLogs(name string) ([]LXDLogEntry, error) {
	// Try fetching the lxc.log file directly via the API.
	raw, err := exec.Command("incus", "query",
		"/1.0/instances/"+name+"/logs/lxc.log").Output()
	if err != nil {
		// Fall back to lxc info --show-log
		raw, err = exec.Command("incus", "info", "--show-log", name).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("lxc logs: %w", err)
		}
		// lxc info --show-log: skip header, start parsing after "Log:"
		return parseLXDLogAfterMarker(string(raw), "Log:"), nil
	}
	return parseLXDLogLines(string(raw)), nil
}

// GetLXDInstanceConsoleLog returns the contents of
// /var/log/incus/<name>/console.log — the boot/console output the kernel
// + init system has written since the last start of a container.
//
// Two paths tried in order:
//
//  1. `incus console <name> --show-log` over the daemon unix socket.
//     Works when the daemon's console buffer is populated.
//  2. `sudo cat /var/log/incus/<name>/console.log` — the file is root-
//     owned mode 0600 so direct read isn't possible from the zfsnas
//     service user. The ZFSNAS_INCUS sudo alias grants either
//     `cat /var/log/incus/*/console.log` (classic sudo, tight) or `cat *`
//     (sudo-rs, wider — required because sudo-rs rejects any prefix
//     before `*`). Either way, the instance name is regex-validated
//     by lxdNameRe above before this command runs, so the broader
//     sudo-rs form isn't reachable via this code path.
//
// When neither path produces output the function returns an empty
// string with no error so the UI can render "(buffer empty)".
func GetLXDInstanceConsoleLog(name string) (string, error) {
	if !lxdNameRe.MatchString(name) {
		return "", fmt.Errorf("invalid instance name")
	}
	// 1. lxc console --show-log
	if out, err := exec.Command("incus", "console", name, "--show-log").CombinedOutput(); err == nil {
		s := string(out)
		if idx := strings.Index(s, "Console log:"); idx >= 0 {
			s = s[idx+len("Console log:"):]
		}
		return strings.TrimLeft(s, "\r\n"), nil
	}
	// 2. sudo cat fallback. Path is host-side, name comes from the
	//    instance list (regex-validated above) so no traversal risk.
	path := "/var/log/incus/" + name + "/console.log"
	out, err := exec.Command("sudo", "/usr/bin/cat", path).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("read console log: %s", msg)
	}
	return strings.TrimLeft(string(out), "\r\n"), nil
}

// lxcLevels is the set of valid liblxc log level tokens.
var lxcLevels = map[string]bool{
	"DEBUG": true, "INFO": true, "NOTICE": true,
	"WARN": true, "WARNING": true, "ERROR": true, "CRITICAL": true,
}

// reLXDLine matches a LXD-daemon log line whose first token is a full ISO timestamp.
var reLXDLine = regexp.MustCompile(
	`^(\d{4}[-/]\d{2}[-/]\d{2}[T ]\d{2}:\d{2}:\d{2}[.\dZ+-]*)\s+(DEBUG|INFO|WARN(?:ING)?|ERROR|CRITICAL|NOTICE)\s+(.+)`)

// reLXCTimestamp matches a liblxc compact timestamp field: 14 digits + optional fraction.
var reLXCTimestamp = regexp.MustCompile(`^\d{14}(?:\.\d+)?$`)

// parseLXDLogLines parses raw log content produced by liblxc or the LXD daemon.
//
// liblxc emits lines with a compact 14-digit timestamp preceded by 1–2 tokens:
//
//	lxc ct-1 20260422231910.228 ERROR    conf - file:func:line - message
//	lxc      20260422231910.401 ERROR    af_unix - file:func:line - message
//
// LXD daemon lines start with an ISO timestamp:
//
//	2024-01-15T10:30:01.000Z INFO message
func parseLXDLogLines(content string) []LXDLogEntry {
	var entries []LXDLogEntry
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		// Try LXD daemon format first (ISO timestamp at start).
		if m := reLXDLine.FindStringSubmatch(line); m != nil {
			entries = append(entries, LXDLogEntry{
				Time:    m[1],
				Level:   normaliseLevel(m[2]),
				Message: strings.TrimSpace(m[3]),
			})
			continue
		}

		// Try liblxc format: scan fields for a 14-digit compact timestamp;
		// the level token immediately follows it, regardless of how many
		// prefix tokens (process name, instance name) appear before.
		if e, ok := parseLXCLine(line); ok {
			entries = append(entries, e)
			continue
		}

		// Unstructured line — store as-is without timestamp.
		entries = append(entries, LXDLogEntry{Level: "INFO", Message: line})
	}
	return entries
}

// parseLXCLine handles liblxc log lines by finding the compact timestamp field
// regardless of how many prefix tokens precede it.
func parseLXCLine(line string) (LXDLogEntry, bool) {
	fields := strings.Fields(line)
	for i, f := range fields {
		if !reLXCTimestamp.MatchString(f) {
			continue
		}
		// Next field must be the log level.
		if i+1 >= len(fields) {
			break
		}
		lvl := strings.ToUpper(fields[i+1])
		if !lxcLevels[lvl] {
			break
		}
		// Rest of the line is everything after the level token.
		// Reconstruct from the original line to preserve spacing in the message.
		afterLevel := line
		for skip := 0; skip <= i+1; skip++ {
			idx := strings.Index(afterLevel, fields[skip])
			if idx >= 0 {
				afterLevel = afterLevel[idx+len(fields[skip]):]
			}
		}
		return LXDLogEntry{
			Time:    parseLXCTimestamp(f),
			Level:   normaliseLevel(lvl),
			Message: lxcMessageOnly(strings.TrimSpace(afterLevel)),
		}, true
	}
	return LXDLogEntry{}, false
}

// parseLXCTimestamp converts compact liblxc timestamp "20260422231910.228"
// to human-readable "2026-04-22 23:19:10.228".
func parseLXCTimestamp(compact string) string {
	ms := ""
	if dot := strings.Index(compact, "."); dot >= 0 {
		ms = compact[dot:]
		compact = compact[:dot]
	}
	if len(compact) < 14 {
		return compact + ms
	}
	return compact[0:4] + "-" + compact[4:6] + "-" + compact[6:8] +
		" " + compact[8:10] + ":" + compact[10:12] + ":" + compact[12:14] + ms
}

// lxcMessageOnly strips the leading "subsystem - file:func:line - " prefix
// that liblxc prepends before the human-readable message text.
// Input example: "conf - ../src/lxc/conf.c:func:3459 - No such file or directory - Failed to…"
// Output:        "No such file or directory - Failed to…"
func lxcMessageOnly(rest string) string {
	// The pattern is: <subsystem> SP "-" SP <location> SP "-" SP <message...>
	// Split on " - " and drop the first two tokens (subsystem, location).
	parts := strings.SplitN(rest, " - ", 3)
	if len(parts) == 3 {
		return strings.TrimSpace(parts[2])
	}
	return strings.TrimSpace(rest)
}

// parseLXDLogAfterMarker skips content before marker then delegates to parseLXDLogLines.
func parseLXDLogAfterMarker(content, marker string) []LXDLogEntry {
	idx := strings.Index(content, marker)
	if idx < 0 {
		return parseLXDLogLines(content)
	}
	return parseLXDLogLines(content[idx+len(marker):])
}

func normaliseLevel(l string) string {
	switch strings.ToUpper(l) {
	case "WARNING":
		return "WARN"
	case "NOTICE":
		return "INFO"
	default:
		return strings.ToUpper(l)
	}
}

// LXDMoveStorage migrates all instance storage from its current pool to targetPool.
// The instance must be stopped. This calls `lxc move <name> --storage <targetPool>`
// which copies all volumes and rebuilds the root disk device entry.
func LXDMoveStorage(name, targetPool string) error {
	if !lxdNameRe.MatchString(name) {
		return fmt.Errorf("invalid instance name")
	}
	if targetPool == "" {
		return fmt.Errorf("target storage pool is required")
	}
	out, err := exec.Command("incus", "move", name, "--storage", targetPool).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}
