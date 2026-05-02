package system

import (
	"bufio"
	"context"
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
	Type        string `json:"type"`         // "virtual-machine" | "container"
	Status      string `json:"status"`       // "Running", "Stopped", "Starting", "Stopping", ...
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

// LXDDisk is an extra virtual disk for a VM.
type LXDDisk struct {
	DeviceName string `json:"device_name"`
	Pool       string `json:"pool"`
	SizeGB     int    `json:"size_gb"`
	ReservePct int    `json:"reserve_pct"` // 0=thin, 25/50/75/100
}

// LXDExistingDisk references an existing ZFS volume to attach as a raw block device.
type LXDExistingDisk struct {
	DeviceName string `json:"device_name"`
	DevPath    string `json:"dev_path"` // /dev/zvol/<pool>/<name>
}

// LXDFreeZVol is a volume available for attachment.
// Raw ZVols: DevPath = "/dev/zvol/…"
// LXD-managed volumes: DevPath = "lxd:<pool>/<volname>"
type LXDFreeZVol struct {
	Name    string `json:"name"`     // display name
	DevPath string `json:"dev_path"` // /dev/zvol/… or lxd:<pool>/<vol>
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
	poolsOut, err := exec.Command("lxc", "query", "/1.0/storage-pools?recursion=1").Output()
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
		volsOut, err := exec.Command("lxc", "query",
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
				exec.Command("lxc", "storage", "volume", "delete", pool.Name, v.Name).Run()
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
	out, err := exec.Command("lxc", "query", "/1.0/storage-pools?recursion=1").Output()
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
	DeviceName string `json:"device_name"`
	Address    string `json:"address"` // e.g. "0000:02:00.0"
	Desc       string `json:"desc"`
	ROMBar     string `json:"rombar,omitempty"`  // "0" or "1"; "" = LXD default
	AER        string `json:"aer,omitempty"`     // "0" or "1"; "" = LXD default
	XVGA       string `json:"x_vga,omitempty"`   // "0" or "1"; "" = LXD default
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
	Name            string         `json:"name"`
	Description     string         `json:"description"`
	Image           string         `json:"image"`
	Profile         string         `json:"profile"`
	AutoStart       bool           `json:"auto_start"`
	VCPU            int            `json:"vcpu"`
	MemoryMB        int            `json:"memory_mb"`
	MemoryHugepages bool           `json:"memory_hugepages"`
	RootPool        string         `json:"root_pool"`
	RootSizeGB      int            `json:"root_size_gb"`
	ExtraDisks      []LXDDisk      `json:"extra_disks"`
	ExistingDisks   []LXDExistingDisk `json:"existing_disks_raw"`
	NICs            []LXDNIC       `json:"nics"`
	USBDevices      []LXDUSBDevice `json:"usb_devices"`
	PCIDevices      []LXDPCIDevice `json:"pci_devices"`
	CloudInit       string         `json:"cloud_init"`
	CDROMPath       string         `json:"cdrom_path"` // absolute path to ISO, "" = no disc
	CDROMPool       string         `json:"cdrom_pool"` // pool name — handler resolves to CDROMPath
	CDROMIso        string         `json:"cdrom_iso"`  // ISO filename within pool's .isos dir
	CDROMs          []string       `json:"cdroms"`     // handler-resolved absolute ISO paths (multi-drive)
	CPUSockets        int    `json:"cpu_sockets"`        // QEMU socket topology (0 = auto)
	CPUPin            string `json:"cpu_pin"`            // LXD limits.cpu range string for pinning
	StatefulSnapshots bool   `json:"stateful_snapshots"` // sets migration.stateful before first start
	Firmware          string `json:"firmware"`           // "uefi" (default) | "bios"
	SecureBoot        bool   `json:"secure_boot"`        // only meaningful when Firmware == "uefi"
	TPM               bool   `json:"tpm"`                // enable emulated TPM 2.0 (security.tpm)
	MachineType       string `json:"machine_type"`       // "" = auto, "pc-q35-9.1", "pc-i440fx-9.1", "q35", "pc", etc.
	DiskBus           string `json:"disk_bus"`           // "" = virtio-blk (default), "scsi", "nvme"
}

// LXDCreateContainerRequest contains all parameters for container creation.
type LXDCreateContainerRequest struct {
	Name         string                 `json:"name"`
	Description  string                 `json:"description"`
	Image        string                 `json:"image"`
	Profile      string                 `json:"profile"`
	AutoStart    bool                   `json:"auto_start"`
	CPUCores     int                    `json:"cpu_cores"`
	CPUShares    int                    `json:"cpu_shares"`    // 1-10, maps to limits.cpu.priority
	CPULimitPct  int                    `json:"cpu_limit_pct"` // 0=unlimited; 1-100 → limits.cpu.allowance
	MemoryMB     int                    `json:"memory_mb"`
	SwapMB       int                    `json:"swap_mb"`     // -1 = no swap, 0 = unlimited, >0 = N MB
	DiskSizeGB   int                    `json:"disk_size_gb"`
	RootPool     string                 `json:"root_pool"`
	Unprivileged bool                   `json:"unprivileged"` // true = security.privileged=false (default)
	Nesting       bool                   `json:"nesting"`       // security.nesting=true
	FeatureKeyctl bool                   `json:"feature_keyctl"` // security.syscalls.allow=keyctl
	FeatureFUSE   bool                   `json:"feature_fuse"`   // adds /dev/fuse device
	RootPassword  string                 `json:"root_password"`  // set via chpasswd after start
	StartupOrder  int                    `json:"startup_order"`
	StartupDelay  int                    `json:"startup_delay"`
	Devices       []LXDPassthroughDevice `json:"devices"`
	NICs          []LXDNIC               `json:"nics"`
}

// LXDInstanceStats holds live resource usage for a running instance.
type LXDInstanceStats struct {
	Status        string  `json:"status"`
	UptimeSec     int64   `json:"uptime_sec"`       // seconds since instance started (0 if unknown)
	CPUUsageNs    int64   `json:"cpu_usage_ns"`
	CPUPct        float64 `json:"cpu_pct"`          // current CPU % across all vCPUs (0-100)
	CPUCount      int     `json:"cpu_count"`         // number of vCPUs configured
	MemUsedBytes  int64   `json:"mem_used_bytes"`
	MemPeakBytes  int64   `json:"mem_peak_bytes"`
	MemLimitBytes int64   `json:"mem_limit_bytes"`   // 0 = unlimited
	DiskUsedBytes int64   `json:"disk_used_bytes"`
	DiskSizeBytes int64   `json:"disk_size_bytes"`   // 0 = unlimited / unknown
	Processes     int     `json:"processes"`
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
		Pid       int64  `json:"pid"`         // host PID of the container init / VM process
		CPU       struct{ Usage int64 `json:"usage"` } `json:"cpu"`
		Memory    struct {
			Usage     int64 `json:"usage"`
			UsagePeak int64 `json:"usage_peak"`
		} `json:"memory"`
		Disk      map[string]struct{ Usage int64 `json:"usage"` } `json:"disk"`
		Processes int `json:"processes"`
	}
	queryState := func() (int64, *stateRaw, error) {
		out, err := exec.Command("lxc", "query", "/1.0/instances/"+name+"/state").Output()
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

	// Disk size: find root disk config.
	diskSize := int64(0)
	for _, d := range cfg.Disks {
		if d.IsRoot {
			diskSize = parseLXDBytes(d.Size)
			break
		}
	}

	disk := int64(0)
	if root, ok := raw2.Disk["root"]; ok {
		disk = root.Usage
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

	memUsed  := raw2.Memory.Usage
	memPeak  := raw2.Memory.UsagePeak
	// VMs without lxd-agent: the /state endpoint reports memory.usage = 0
	// and cpu.usage stuck at 0 because the cgroup doesn't see inside the
	// QEMU process. LXD's prom endpoint reports real values for both via
	// QEMU's balloon driver and the host-side qemu cgroup, so fall back
	// to it whenever /state has nothing useful. The Monitor tab's
	// realtime row uses the same source via GetLXDInstanceRealtime;
	// piggy-backing on its rate cache here means the Live Info card,
	// when it polls every 3 s, reuses whatever baseline the realtime row
	// last established.
	if memUsed == 0 {
		if used, total := lxdPromMemoryFor(name); used > 0 {
			memUsed = used
			if memLimit == 0 && total > 0 {
				memLimit = total
			}
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

	return LXDInstanceStats{
		Status:        raw2.Status,
		UptimeSec:     uptimeSec,
		CPUUsageNs:    raw2.CPU.Usage,
		CPUPct:        cpuPct,
		CPUCount:      cpuCount,
		MemUsedBytes:  memUsed,
		MemPeakBytes:  memPeak,
		MemLimitBytes: memLimit,
		DiskUsedBytes: disk,
		DiskSizeBytes: diskSize,
		Processes:     raw2.Processes,
	}, nil
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
// Returns a map of normalised PCI address → option map for -device vfio-pci entries.
func parsePCIQEMUArgs(rawQEMU string) map[string]map[string]string {
	result := map[string]map[string]string{}
	re := regexp.MustCompile(`-device\s+vfio-pci,([^\s]+)`)
	for _, m := range re.FindAllStringSubmatch(rawQEMU, -1) {
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
	return result
}

// buildPCIQEMUArg returns a -device vfio-pci,... string for a PCI device that
// has at least one extra option (rombar/x-vga/aer) set. Returns "" otherwise.
func buildPCIQEMUArg(pci LXDPCIDevice) string {
	if pci.ROMBar == "" && pci.XVGA == "" && pci.AER == "" {
		return ""
	}
	parts := []string{"host=" + normPCIAddr(pci.Address)}
	if pci.ROMBar != "" {
		parts = append(parts, "rombar="+pci.ROMBar)
	}
	if pci.XVGA != "" {
		parts = append(parts, "x-vga="+pci.XVGA)
	}
	if pci.AER != "" {
		parts = append(parts, "aer="+pci.AER)
	}
	return "-device vfio-pci," + strings.Join(parts, ",")
}

// applyPCIRawQEMU rewrites the instance raw.qemu config key to include
// per-device vfio-pci overrides. All existing -device vfio-pci entries are
// removed and replaced with entries derived from pciDevices (only those with
// at least one option set are written back). Other raw.qemu content is kept.
func applyPCIRawQEMU(name string, pciDevices []LXDPCIDevice) {
	out, _ := exec.Command("lxc", "config", "get", name, "raw.qemu").Output()
	existing := strings.TrimSpace(string(out))

	// Strip all existing -device vfio-pci entries (ZNAS fully manages them).
	re := regexp.MustCompile(`\s*-device\s+vfio-pci,[^\s]*`)
	existing = strings.TrimSpace(re.ReplaceAllString(existing, ""))

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
		exec.Command("lxc", "config", "unset", name, "raw.qemu").Run()
	} else {
		exec.Command("lxc", "config", "set", name, "raw.qemu", newVal).Run()
	}
}

// LXDAvailable probes LXD accessibility by running `lxc list --format json`.
func LXDAvailable() bool {
	cmd := exec.Command("lxc", "list", "--format", "json")
	return cmd.Run() == nil
}

// LXDVersion returns the lxc client version string.
func LXDVersion() string {
	out, err := exec.Command("lxc", "version").Output()
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
	out, err := exec.Command("lxc", "list", "--format", "json").Output()
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
	out, err := exec.Command("lxc", "list", name, "--format", "json").Output()
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
	out, err := exec.Command("lxc", "start", name).CombinedOutput()
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
	out, err := exec.Command("lxc", args...).CombinedOutput()
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
	out, err := exec.Command("lxc", "restart", name).CombinedOutput()
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
	return exec.Command("lxc", "delete", "--force", name).Run()
}

// LXDNICConfig describes a network interface device on an instance.
type LXDNICConfig struct {
	Name        string `json:"name"`
	Bridge      string `json:"bridge"`              // "network" or "parent" value
	NICType     string `json:"nictype"`             // "network" (managed) or "bridged" (direct bridge)
	Connected   bool   `json:"connected"`           // false when OS link is down (detected from instance state)
	VlanID      int    `json:"vlan_id,omitempty"`
	FromProfile bool   `json:"from_profile,omitempty"`
	MAC         string `json:"mac,omitempty"`        // volatile.<name>.hwaddr
	CurrentIP   string `json:"current_ip,omitempty"` // live IPv4 from instance state
	IPv4Mode    string `json:"ipv4_mode,omitempty"`  // "dhcp" | "static" | "none"
	IPv4Addr    string `json:"ipv4_addr,omitempty"`  // e.g. "10.0.0.10/24"
	IPv4GW      string `json:"ipv4_gw,omitempty"`    // gateway IP
	DNS1        string `json:"dns1,omitempty"`        // primary DNS (static only)
	DNS2        string `json:"dns2,omitempty"`        // secondary DNS (static only)
}

// LXDDiskConfig describes a disk device on an instance.
type LXDDiskConfig struct {
	Name         string `json:"name"`
	Pool         string `json:"pool,omitempty"`
	Size         string `json:"size,omitempty"`
	ReservePct   int    `json:"reserve_pct,omitempty"` // 0=thin, 25/50/75/100
	IsRoot       bool   `json:"is_root,omitempty"`
	FromProfile  bool   `json:"from_profile,omitempty"`
	ZFSPath      string `json:"zfs_path,omitempty"`      // backing ZFS path
	ZFSType      string `json:"zfs_type,omitempty"`      // "zvol" | "dataset"
	CompRatio    string `json:"comp_ratio,omitempty"`    // e.g. "1.23x"
	BootPriority string `json:"boot_priority,omitempty"` // lxd boot.priority value
	// VM-only per-disk knobs. Empty string means "leave unset / inherit".
	IOCache  string `json:"io_cache,omitempty"`  // "" | "none" | "writeback" | "writethrough" | "unsafe" | "directsync"
	IOBus    string `json:"io_bus,omitempty"`    // "" | "virtio-blk" | "virtio-scsi" | "nvme" — overrides the VM-wide DiskBus when non-empty
	ReadOnly bool   `json:"readonly,omitempty"`  // attach disk read-only
}

// LXDCapabilities flags optional disk knobs by LXD's API extension list, so
// the UI can render only the keys the running LXD will actually accept
// (LXD 5.0.x lacks `io.bus`/`io.threads`, breaks the dropdown silently).
type LXDCapabilities struct {
	DiskIOBus    bool `json:"disk_io_bus"`
	DiskIOCache  bool `json:"disk_io_cache"`
	DiskIOThreads bool `json:"disk_io_threads"`
}

// LXDInstanceConfig holds the editable configuration of an LXD instance.
type LXDInstanceConfig struct {
	Description        string                 `json:"description"`
	CPULimit           string                 `json:"cpu_limit"`
	CPUPin             string                 `json:"cpu_pin"`            // LXD range string for pinning; overrides CPULimit when non-empty
	CPUSockets          int                    `json:"cpu_sockets"`        // QEMU socket topology (0=auto)
	MemoryLimit         string                 `json:"memory_limit"`
	MemoryHugepages     bool                   `json:"memory_hugepages"`
	MemoryReservation   string                 `json:"memory_reservation"` // "", "25", "50", "75", "100", or "custom:<size>"
	Nesting              bool                   `json:"nesting"`
	Autostart            bool                   `json:"autostart"`
	StatefulSnapshots    bool                   `json:"stateful_snapshots"` // migration.stateful — VM-only
	IsVM                 bool                   `json:"is_vm"`
	// Container-specific features (only applied when ApplyContainerFeatures is true)
	ApplyContainerFeatures bool   `json:"apply_container_features,omitempty"`
	CPULimitPct            int    `json:"cpu_limit_pct,omitempty"`  // 0=unset, 1-100 → limits.cpu.allowance
	CPUShares              int    `json:"cpu_shares,omitempty"`     // 0=unset, 1-10 → limits.cpu.priority
	SwapLimit              string `json:"swap_limit,omitempty"`     // "" | "false" | "512MB"
	Unprivileged           bool   `json:"unprivileged,omitempty"`   // security.privileged = !Unprivileged
	FeatureKeyctl          bool   `json:"feature_keyctl,omitempty"` // security.syscalls.allow=keyctl
	FeatureFUSE            bool   `json:"feature_fuse,omitempty"`   // /dev/fuse device
	CDROMPath          string                 `json:"cdrom_path"`    // current ISO path (GET) / desired path (PUT)
	ApplyCDROM         bool                   `json:"apply_cdrom"`   // if true, apply CDROMPath change on PUT (legacy single-drive)
	CDROMs             []string               `json:"cdroms"`        // handler-resolved absolute ISO paths (multi-drive)
	ApplyCDROMs        bool                   `json:"apply_cdroms"`  // if true, replace all CDROMs with CDROMs list
	Firmware           string                 `json:"firmware"`      // "uefi" (default) | "bios"
	SecureBoot         bool                   `json:"secure_boot"`   // only meaningful when Firmware == "uefi"
	TPM                bool                   `json:"tpm"`           // enable emulated TPM 2.0 (security.tpm)
	MachineType        string                 `json:"machine_type"`  // "" = auto, "pc-q35-9.1", "pc-i440fx-9.1", etc.
	DiskBus            string                 `json:"disk_bus"`      // "" = virtio-blk (default), "scsi", "nvme"
	NICs               []LXDNICConfig         `json:"nics"`
	Disks              []LXDDiskConfig        `json:"disks"`
	DetachDisks        []string               `json:"detach_disks,omitempty"` // device names to detach only (keep backing volume)
	ExistingDisks      []LXDExistingDisk      `json:"existing_disks_raw"` // ZVols to attach as new raw block devices
	USBDevices         []LXDUSBDevice         `json:"usb_devices"`
	PCIDevices         []LXDPCIDevice         `json:"pci_devices"`
	PassthroughDevices []LXDPassthroughDevice `json:"passthrough_devices"`
	// Daemon-side capability flags (read-only on GET; ignored on PUT). Lets
	// the editor disable inputs that the running LXD will silently reject.
	Capabilities       LXDCapabilities        `json:"capabilities,omitempty"`
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

// cdromDriveRe matches -drive ...media=cdrom... entries in a raw.qemu string.
// QEMU drive values are comma-separated with no internal spaces (file paths with spaces are
// not valid in LXD ISO names), so \S+ captures the entire value without crossing into other flags.
var cdromDriveRe = regexp.MustCompile(`\s*-drive\s+\S*media=cdrom\S*`)

// cdromFileRe extracts the file= path from a -drive media=cdrom entry.
var cdromFileRe = regexp.MustCompile(`-drive\s+file=([^,\s]+)[^-]*media=cdrom`)

// cdromAAre matches AppArmor rules added by ZNAS for ISO directories.
// Covers both old-style /.isos/ rules and new-style snap isos rules.
var cdromAAre = regexp.MustCompile(`\S+/(?:\.isos|isos/[^/]+)/\*\* rk,`)

// setCDROMsRawQEMU replaces any existing cdrom -drive entries in rawQemu with entries for
// the given ISO paths.  In QEMU Q35 machines, if=ide maps to the ICH9 AHCI controller
// (SATA), which Windows natively supports without virtio drivers.
func setCDROMsRawQEMU(rawQemu string, paths []string) string {
	rawQemu = strings.TrimSpace(cdromDriveRe.ReplaceAllString(rawQemu, ""))
	for i, p := range paths {
		if p == "" || !filepath.IsAbs(p) {
			continue
		}
		rawQemu = strings.TrimSpace(rawQemu) +
			fmt.Sprintf(" -drive file=%s,if=ide,media=cdrom,readonly=on,index=%d", p, i)
	}
	return strings.TrimSpace(rawQemu)
}

// setCDROMsAppArmor replaces ISO directory AppArmor rules in rawAA with the appropriate
// rules for the given ISO paths.
//
// Snap LXD: ISOs are under /var/snap/lxd/common/lxd/isos/ — one blanket rule covers all pools.
// Non-snap: per-directory rules for each unique ISO parent directory.
func setCDROMsAppArmor(rawAA string, paths []string) string {
	rawAA = strings.TrimSpace(cdromAAre.ReplaceAllString(rawAA, ""))
	if lxdIsSnap() {
		// One rule covers all pools; individual pool subdirs are already inside this tree.
		hasISO := false
		for _, p := range paths {
			if p != "" && filepath.IsAbs(p) {
				hasISO = true
				break
			}
		}
		if hasISO {
			rawAA = strings.TrimSpace(rawAA) + " " + snapLXDISOBase + "/** rk,"
		}
	} else {
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

// vmApplyCDROMs updates raw.qemu and raw.apparmor on a VM for the given ISO paths,
// and removes any legacy LXD cdrom disk devices (virtio-scsi) that may already exist.
func vmApplyCDROMs(name string, paths []string, applyConf func(string, string) error) {
	// Remove legacy LXD cdrom disk devices so we don't present duplicate drives.
	out, _ := exec.Command("lxc", "query", "/1.0/instances/"+name).Output()
	var inst struct {
		Devices map[string]map[string]string `json:"devices"`
	}
	if json.Unmarshal(out, &inst) == nil {
		for devName, d := range inst.Devices {
			if d["type"] == "disk" && d["readonly"] == "true" &&
				strings.HasSuffix(strings.ToLower(d["source"]), ".iso") {
				exec.Command("lxc", "config", "device", "remove", name, devName).Run() //nolint:errcheck
			}
		}
	}

	// Read current raw.qemu (may already have socket topology or PCI flags) and update CDROMs.
	curRQ, _ := exec.Command("lxc", "config", "get", name, "raw.qemu").Output()
	newRQ := setCDROMsRawQEMU(strings.TrimSpace(string(curRQ)), paths)
	// raw.qemu values start with '-' which lxc config set misparses as its own flags.
	// Use lxc query PATCH to set the value safely.
	lxdPatchConfig(name, "raw.qemu", newRQ)

	// Update raw.apparmor so snapped LXD's QEMU sandbox can open the ISO files.
	curAA, _ := exec.Command("lxc", "config", "get", name, "raw.apparmor").Output()
	newAA := setCDROMsAppArmor(strings.TrimSpace(string(curAA)), paths)
	applyConf("raw.apparmor", newAA) //nolint:errcheck
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
		out, err := exec.Command("lxc", "query", "/1.0").Output()
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
		out, err := exec.Command("lxc", "query", "/1.0/metadata/configuration").Output()
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
		exec.Command("lxc", "config", "unset", name, key).Run() //nolint:errcheck
		return
	}
	exec.Command("lxc", "config", "set", name, key+"="+val).Run() //nolint:errcheck
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
	out, err := exec.Command("lxc", "query", "/1.0/storage-pools/"+lxdPool).Output()
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
	out, err := exec.Command("lxc", "query", "/1.0/instances/"+name).Output()
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
		StatefulSnapshots: raw.Config["migration.stateful"] == "true",
		IsVM:              raw.Type == "virtual-machine",
		Firmware:          firmware,
		SecureBoot:        secureBoot,
		MachineType:       raw.Config["qemu.machine.type"],
		// Container-specific (populated for containers, ignored for VMs)
		CPULimitPct:   cpuLimitPct,
		CPUShares:     cpuShares,
		SwapLimit:     raw.Config["limits.memory.swap"],
		Unprivileged:  raw.Config["security.privileged"] != "true",
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
			disk := LXDDiskConfig{
				Name:         devName,
				Pool:         lxdPool,
				Size:         lxdNormalizeSizeStr(devCfg["size"]),
				IsRoot:       isRoot,
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
	stateOut, err := exec.Command("lxc", "query", "/1.0/instances/"+name+"/state").Output()
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
			if opts, ok := qemuOpts[normPCIAddr(pci.Address)]; ok {
				cfg.PCIDevices[i].ROMBar = opts["rombar"]
				cfg.PCIDevices[i].XVGA = opts["x-vga"]
				cfg.PCIDevices[i].AER = opts["aer"]
			}
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
	out, _ := exec.Command("lxc", "config", "get", name, "raw.qemu").Output()
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
		setOut, setErr = exec.Command("lxc", "config", "unset", name, "raw.qemu").CombinedOutput()
	} else {
		// Use key=value form so lxc doesn't parse the leading "-global" as its own flag.
		setOut, setErr = exec.Command("lxc", "config", "set", name, "raw.qemu="+cleaned).CombinedOutput()
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

	// Resolve IsVM from LXD if the caller did not supply it.
	if !cfg.IsVM {
		if out, err := exec.Command("lxc", "query", "/1.0/instances/"+name).Output(); err == nil {
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
	if out, err := exec.Command("lxc", "query", "-X", "PATCH",
		"/1.0/instances/"+name, "--data", fmt.Sprintf(`{"description":%s}`, descJSON)).CombinedOutput(); err != nil {
		return fmt.Errorf("description: %s", strings.TrimSpace(string(out)))
	}

	// CPU / memory / autostart via lxc config set.
	applyConf := func(key, val string) error {
		var out []byte
		var err error
		if val == "" {
			out, err = exec.Command("lxc", "config", "unset", name, key).CombinedOutput()
			if err != nil && strings.Contains(string(out), "not currently set") {
				return nil
			}
		} else {
			// Use key=value form when value starts with "-" to prevent lxc from
		// interpreting it as its own CLI flag (e.g. raw.qemu=-global ...).
		if strings.HasPrefix(val, "-") {
			out, err = exec.Command("lxc", "config", "set", name, key+"="+val).CombinedOutput()
		} else {
			out, err = exec.Command("lxc", "config", "set", name, key, val).CombinedOutput()
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
		if out, err := exec.Command("lxc", "config", "get", name, "raw.qemu").Output(); err == nil {
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
	// migration.stateful — VM-only; controls whether QEMU is initialised in a way
	// that supports stateful (memory-including) snapshots. Can only be changed while
	// the instance is stopped; ignore the error here so other settings still apply
	// (the UI already warns the user about the stop requirement).
	if cfg.IsVM {
		// TPM and migration.stateful are mutually exclusive in LXD.
		wantStateful := cfg.StatefulSnapshots && !cfg.TPM
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
		if out, err := exec.Command("lxc", "query", "/1.0/instances/"+name).Output(); err == nil {
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
			exec.Command("lxc", "config", "device", "add", name, "tpm", "tpm").Run() //nolint:errcheck
		} else if !cfg.TPM && hasTPM {
			exec.Command("lxc", "config", "device", "remove", name, "tpm").Run() //nolint:errcheck
		}
	}
	// Machine type (VM-only). Empty string unsets the override, letting LXD choose.
	if cfg.IsVM {
		applyConf("qemu.machine.type", cfg.MachineType) //nolint:errcheck
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
		exec.Command("lxc", "config", "unset", name, "security.syscalls.allow").Run() //nolint:errcheck
		if cfg.FeatureKeyctl {
			if lxdSupportsConfigKey("security.syscalls.intercept.keyctl") {
				if err := applyConf("security.syscalls.intercept.keyctl", "true"); err != nil {
					return err
				}
			}
		} else {
			exec.Command("lxc", "config", "unset", name, "security.syscalls.intercept.keyctl").Run() //nolint:errcheck
		}
	}

	// Fetch current instance-level devices for diff.
	rawOut, err := exec.Command("lxc", "query", "/1.0/instances/"+name).Output()
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
		out, err := exec.Command("lxc", args...).CombinedOutput()
		if err != nil && strings.Contains(string(out), "DNS name") {
			exec.Command("lxc", "network", "set", bridge, "dns.mode=none").Run() //nolint:errcheck
			out, err = exec.Command("lxc", args...).CombinedOutput()
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
					exec.Command("lxc", "exec", name, "--", "ip", "link", "set", nic.Name, "down").Run()
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
				profDev    := expandedNICs[nic.Name]
				profBridge := profDev["network"]
				if profBridge == "" {
					profBridge = profDev["parent"]
				}
				profVlan := profDev["vlan"]
				profMAC  := strings.ToLower(profDev["hwaddr"])
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
					exec.Command("lxc", "exec", name, "--", "ip", "link", "set", nic.Name, "up").Run()
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
			exec.Command("lxc", "config", "unset", name, "user.disconnected_nics."+nic.Name).Run() //nolint:errcheck
		} else {
			// NIC exists in instance config.
			// Clear any stale disconnected-NIC metadata so the UI stays consistent.
			exec.Command("lxc", "config", "unset", name, "user.disconnected_nics."+nic.Name).Run() //nolint:errcheck

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
				if out, err := exec.Command("lxc", "config", "device", "remove", name, nic.Name).CombinedOutput(); err != nil {
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
				exec.Command("lxc", "config", "set", name,
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
				if out, err := exec.Command("lxc", "config", "device", "remove", name, nic.Name).CombinedOutput(); err != nil {
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
					exec.Command("lxc", "config", "device", "unset", name, nic.Name, "vlan").Run() //nolint:errcheck
				} else {
					if out, err := exec.Command("lxc", "config", "device", "set",
						name, nic.Name, "vlan="+wantVlan).CombinedOutput(); err != nil {
						return fmt.Errorf("update NIC %s vlan: %s", nic.Name, strings.TrimSpace(string(out)))
					}
				}
			}
			curMAC := strings.ToLower(cur["hwaddr"])
			wantMAC := strings.ToLower(nic.MAC)
			if curMAC != wantMAC {
				if wantMAC == "" {
					exec.Command("lxc", "config", "device", "unset", name, nic.Name, "hwaddr").Run() //nolint:errcheck
				} else {
					if out, err := exec.Command("lxc", "config", "device", "set",
						name, nic.Name, "hwaddr="+wantMAC).CombinedOutput(); err != nil {
						return fmt.Errorf("update NIC %s hwaddr: %s", nic.Name, strings.TrimSpace(string(out)))
					}
				}
			}
		}
	}
	for n := range curNICs {
		if _, ok := wantNICs[n]; !ok {
			if out, err := exec.Command("lxc", "config", "device", "remove", name, n).CombinedOutput(); err != nil {
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
				exec.Command("lxc", "exec", name, "--", "ip", "link", "set", nic.Name, "up").Run() //nolint:errcheck
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
				cmd := exec.Command("lxc", "exec", name, "--", "/bin/sh", "-c",
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
			if out, err := exec.Command("lxc", volArgs...).CombinedOutput(); err != nil {
				return fmt.Errorf("create volume for %s: %s", disk.Name, strings.TrimSpace(string(out)))
			}
			devArgs := []string{"config", "device", "add", name, disk.Name, "disk",
				"pool=" + disk.Pool, "source=" + volName}
			if out, err := exec.Command("lxc", devArgs...).CombinedOutput(); err != nil {
				exec.Command("lxc", "storage", "volume", "delete", disk.Pool, volName).Run()
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
			} else if out, err := exec.Command("lxc", "config", "device", "set", name, disk.Name, "size", disk.Size).CombinedOutput(); err != nil {
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
					exec.Command("lxc", "config", "device", "unset", name, disk.Name, "boot.priority").Run()
				} else {
					exec.Command("lxc", "config", "device", "set", name, disk.Name, "boot.priority", disk.BootPriority).Run()
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
						exec.Command("lxc", "config", "device", "unset", name, disk.Name, "io.bus").Run() //nolint:errcheck
					} else {
						exec.Command("lxc", "config", "device", "set", name, disk.Name, "io.bus", want).Run() //nolint:errcheck
					}
				}
			}
			// io.cache (LXD ≥ 5.0 with disk_io_cache extension; widely available).
			if isVM && caps.DiskIOCache && disk.IOCache != cur["io.cache"] {
				if disk.IOCache == "" {
					exec.Command("lxc", "config", "device", "unset", name, disk.Name, "io.cache").Run() //nolint:errcheck
				} else {
					exec.Command("lxc", "config", "device", "set", name, disk.Name, "io.cache", disk.IOCache).Run() //nolint:errcheck
				}
			}
			// readonly: skip on root disks (LXD rejects readonly=true on /).
			if !disk.IsRoot {
				curRO := cur["readonly"] == "true"
				if curRO != disk.ReadOnly {
					if disk.ReadOnly {
						exec.Command("lxc", "config", "device", "set", name, disk.Name, "readonly", "true").Run() //nolint:errcheck
					} else {
						exec.Command("lxc", "config", "device", "unset", name, disk.Name, "readonly").Run() //nolint:errcheck
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
					exec.Command("lxc", "config", "device", "set", name, disk.Name, "io.bus", want).Run() //nolint:errcheck
				}
			}
			if caps.DiskIOCache && disk.IOCache != "" {
				exec.Command("lxc", "config", "device", "set", name, disk.Name, "io.cache", disk.IOCache).Run() //nolint:errcheck
			}
			if !disk.IsRoot && disk.ReadOnly {
				exec.Command("lxc", "config", "device", "set", name, disk.Name, "readonly", "true").Run() //nolint:errcheck
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
			if out, err := exec.Command("lxc", "config", "device", "remove", name, n).CombinedOutput(); err != nil {
				return fmt.Errorf("remove disk %s: %s", n, strings.TrimSpace(string(out)))
			}
			// Delete the backing block volume unless this is a detach-only operation.
			if !detachOnly[n] && volPool != "" && volName == name+"-"+n {
				exec.Command("lxc", "storage", "volume", "delete", volPool, volName).Run()
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
					if err := exec.Command("lxc", "stop", name, "--timeout=20").Run(); err != nil {
						exec.Command("lxc", "stop", name, "--force").Run() //nolint:errcheck
					}
				}
				if cfg.DiskBus == "" {
					exec.Command("lxc", "config", "device", "unset", name, rootName, "io.bus").Run() //nolint:errcheck
				} else if rootIsInstance {
					exec.Command("lxc", "config", "device", "set", name, rootName, "io.bus", cfg.DiskBus).Run() //nolint:errcheck
				} else {
					// Profile-inherited root: 'override' creates an instance-level copy with io.bus set.
					exec.Command("lxc", "config", "device", "override", name, rootName, "io.bus="+cfg.DiskBus).Run() //nolint:errcheck
				}
				if needStop {
					exec.Command("lxc", "start", name).Run() //nolint:errcheck
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
					exec.Command("lxc", "config", "device", "remove", name, n).Run() //nolint:errcheck
				}
			}
			for i, path := range cfg.CDROMs {
				if path == "" || !filepath.IsAbs(path) {
					continue
				}
				devName := fmt.Sprintf("cdrom%d", i)
				exec.Command("lxc", "config", "device", "add", name, devName, "disk", //nolint:errcheck
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
					exec.Command("lxc", "config", "device", "remove", name, n).Run() //nolint:errcheck
					break
				}
			}
			if cfg.CDROMPath != "" {
				exec.Command("lxc", "config", "device", "add", name, "cdrom", "disk", //nolint:errcheck
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
		if out, err := exec.Command("lxc", dArgs...).CombinedOutput(); err != nil {
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
				exec.Command("lxc", "config", "device", "remove", name, usb.DeviceName).Run()
			}
			args := []string{"config", "device", "add", name, usb.DeviceName, "usb",
				"vendorid=" + usb.VendorID, "productid=" + usb.ProductID}
			if out, err := exec.Command("lxc", args...).CombinedOutput(); err != nil {
				return fmt.Errorf("add USB %s: %s", usb.DeviceName, strings.TrimSpace(string(out)))
			}
		}
	}
	for n := range curUSB {
		if _, ok := wantUSB[n]; !ok {
			exec.Command("lxc", "config", "device", "remove", name, n).Run()
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
				exec.Command("lxc", "config", "device", "remove", name, pci.DeviceName).Run()
			}
			args := []string{"config", "device", "add", name, pci.DeviceName, "pci", "address=" + addr}
			if out, err := exec.Command("lxc", args...).CombinedOutput(); err != nil {
				return fmt.Errorf("add PCI %s: %s", pci.DeviceName, strings.TrimSpace(string(out)))
			}
		}
	}
	for n := range curPCI {
		if _, ok := wantPCI[n]; !ok {
			exec.Command("lxc", "config", "device", "remove", name, n).Run()
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
			exec.Command("lxc", "config", "device", "remove", name, dev.DeviceName).Run()
		}
		args := []string{"config", "device", "add", name, dev.DeviceName, dev.Type}
		if dev.HostPath != "" {
			args = append(args, "path="+dev.HostPath)
		}
		for k, v := range dev.Extra {
			args = append(args, k+"="+v)
		}
		if out, err := exec.Command("lxc", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("add device %s: %s", dev.DeviceName, strings.TrimSpace(string(out)))
		}
	}
	for n := range curPassthrough {
		if _, ok := wantPT[n]; !ok {
			exec.Command("lxc", "config", "device", "remove", name, n).Run()
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
			exec.Command("lxc", "config", "device", "add", name, "fuse", "unix-char", "path=/dev/fuse").Run()
		} else if !cfg.FeatureFUSE && fuseExists {
			exec.Command("lxc", "config", "device", "remove", name, "fuse").Run()
		}
	}

	// Apply rombar/x-vga/aer via raw.qemu (LXD pci device type does not accept them directly).
	applyPCIRawQEMU(name, cfg.PCIDevices)

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
	out, err := exec.Command("lxc", "profile", "list", "--format", "json").Output()
	if err != nil {
		return nil, err
	}
	var raw []struct{ Name string `json:"name"` }
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
	out, err := exec.Command("lxc", "storage", "list", "--format", "json").Output()
	if err != nil {
		return nil, err
	}
	var raw []struct{ Name string `json:"name"` }
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
	out, err := exec.Command("lxc", "network", "list", "--format", "json").Output()
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
	out, err := exec.Command("lxc", "remote", "list", "--format", "json").Output()
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
	out, err := exec.Command("lxc", args...).Output()
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
	out, err := exec.Command("lxc", args...).Output()
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
		args = append(args, "-c", fmt.Sprintf("limits.memory=%dMB", req.MemoryMB))
	}
	if req.MemoryHugepages {
		args = append(args, "-c", "limits.memory.hugepages=true")
	}
	// migration.stateful is set AFTER extra disks are attached (below) because LXD
	// rejects adding non-shared-pool disks while migration.stateful=true is already set.
	if req.AutoStart {
		args = append(args, "-c", "boot.autostart=true")
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
		sizeBytes := int64(req.RootSizeGB) * 1000 * 1000 * 1000
		aligned := ((sizeBytes+headerBytes+blockSize-1)/blockSize)*blockSize - headerBytes
		args = append(args, "-d", fmt.Sprintf("root,size=%dB", aligned))
	}
	if req.DiskBus != "" {
		args = append(args, "-d", "root,io.bus="+req.DiskBus)
	}
	log("Initialising VM " + req.Name + "…")
	if out, err := exec.Command("lxc", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("lxc init: %s: %w", strings.TrimSpace(string(out)), err)
	}
	// Apply secure boot setting post-init so we can fall back to raw.qemu on LXD 6.x
	// (which removed the security.secureboot config key).
	if req.Firmware == "bios" {
		// BIOS: disable secureboot; ignore error if key not supported by this LXD version.
		if out, err := exec.Command("lxc", "config", "set", req.Name, "security.secureboot=false").CombinedOutput(); err != nil {
			if !strings.Contains(strings.TrimSpace(string(out)), "isn't supported") {
				log("WARNING: could not set security.secureboot=false: " + strings.TrimSpace(string(out)))
			}
		}
	} else if !req.SecureBoot {
		// UEFI without Secure Boot: try the key first, fall back to raw.qemu pflash flag.
		out, err := exec.Command("lxc", "config", "set", req.Name, "security.secureboot=false").CombinedOutput()
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

	// TPM device — add after init.
	if req.TPM {
		if out, err := exec.Command("lxc", "config", "device", "add", req.Name, "tpm", "tpm").CombinedOutput(); err != nil {
			log("WARNING: could not add TPM device: " + strings.TrimSpace(string(out)))
		}
	}

	// Machine type — set after init so qemu.machine.type is available.
	if req.MachineType != "" {
		if out, err := exec.Command("lxc", "config", "set", req.Name, "qemu.machine.type", req.MachineType).CombinedOutput(); err != nil {
			log("WARNING: could not set machine type: " + strings.TrimSpace(string(out)))
		}
	}

	// Set description (display name).
	if req.Description != "" {
		descJSON, _ := json.Marshal(req.Description)
		exec.Command("lxc", "query", "-X", "PATCH", "/1.0/instances/"+req.Name,
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
			"--type", "block", fmt.Sprintf("size=%dGB", disk.SizeGB)}
		log("Adding disk " + devName + "…")
		if out, err := exec.Command("lxc", volArgs...).CombinedOutput(); err != nil {
			log("WARNING: create volume for " + devName + ": " + strings.TrimSpace(string(out)))
			continue
		}
		dArgs := []string{"config", "device", "add", req.Name, devName, "disk",
			"pool=" + disk.Pool, "source=" + volName}
		if req.DiskBus != "" {
			dArgs = append(dArgs, "io.bus="+req.DiskBus)
		}
		if out, err := exec.Command("lxc", dArgs...).CombinedOutput(); err != nil {
			log("WARNING: add disk " + devName + ": " + strings.TrimSpace(string(out)))
			exec.Command("lxc", "storage", "volume", "delete", disk.Pool, volName).Run()
		} else if disk.ReservePct > 0 {
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
		if out, err := exec.Command("lxc", dArgs...).CombinedOutput(); err != nil {
			log("WARNING: attach existing disk " + devName + ": " + strings.TrimSpace(string(out)))
		}
	}

	// Set migration.stateful now that all disks are attached.  Setting it during
	// lxc init causes LXD to reject any subsequent disk-add for non-shared pools.
	if req.StatefulSnapshots && !req.TPM {
		if out, err := exec.Command("lxc", "config", "set", req.Name, "migration.stateful=true").CombinedOutput(); err != nil {
			log("WARNING: set migration.stateful: " + strings.TrimSpace(string(out)))
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
		log("Adding NIC " + devName + "…")
		if out, err := exec.Command("lxc", nArgs...).CombinedOutput(); err != nil {
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
		if out, err := exec.Command("lxc", uArgs...).CombinedOutput(); err != nil {
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
		if out, err := exec.Command("lxc", pArgs...).CombinedOutput(); err != nil {
			log("WARNING: add PCI " + pci.DeviceName + ": " + strings.TrimSpace(string(out)))
		}
	}

	// Apply rombar/x-vga/aer via raw.qemu before starting.
	applyPCIRawQEMU(req.Name, req.PCIDevices)

	// Apply CPU pinning (overrides vCPU count when set).
	if req.CPUPin != "" {
		exec.Command("lxc", "config", "set", req.Name, "limits.cpu", normalizeCPUPin(req.CPUPin)).Run()
	}

	// Apply socket topology via raw.qemu.
	if req.CPUSockets > 0 {
		out, _ := exec.Command("lxc", "config", "get", req.Name, "raw.qemu").Output()
		cur := strings.TrimSpace(string(out))
		next := updateRawQEMUSockets(cur, req.CPUSockets)
		if next != cur {
			if next == "" {
				exec.Command("lxc", "config", "unset", req.Name, "raw.qemu").Run()
			} else {
				exec.Command("lxc", "config", "set", req.Name, "raw.qemu", next).Run()
			}
		}
	}

	// Attach CD/DVD drives via raw.qemu (if=ide → ICH9 AHCI/SATA in Q35).
	// This avoids the virtio-scsi driver requirement so the Windows installer
	// can see both discs immediately.  raw.apparmor is set to allow the ISO dir.
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
		noop := func(k, v string) error { return nil } // placeholder — we call exec directly below
		_ = noop
		// Read current raw.qemu (may have socket topology) and inject CDROM entries.
		// Use lxc query PATCH instead of lxc config set to avoid flag-parsing issues
		// with values starting with '-'.
		curRQ, _ := exec.Command("lxc", "config", "get", req.Name, "raw.qemu").Output()
		newRQ := setCDROMsRawQEMU(strings.TrimSpace(string(curRQ)), paths)
		lxdPatchConfig(req.Name, "raw.qemu", newRQ)
		// AppArmor: allow the ISO directories for snapped LXD.
		curAA, _ := exec.Command("lxc", "config", "get", req.Name, "raw.apparmor").Output()
		newAA := setCDROMsAppArmor(strings.TrimSpace(string(curAA)), paths)
		if newAA != "" {
			exec.Command("lxc", "config", "set", req.Name, "raw.apparmor", newAA).Run() //nolint:errcheck
		}
	}

	if req.AutoStart {
		log("Starting VM…")
		if out, err := exec.Command("lxc", "start", req.Name).CombinedOutput(); err != nil {
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
		args = append(args, "-c", fmt.Sprintf("limits.memory=%dMB", req.MemoryMB))
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
	if out, err := exec.Command("lxc", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("lxc init: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Set description (display name).
	if req.Description != "" {
		descJSON, _ := json.Marshal(req.Description)
		exec.Command("lxc", "query", "-X", "PATCH", "/1.0/instances/"+req.Name,
			"--data", fmt.Sprintf(`{"description":%s}`, descJSON)).Run()
	}

	// Root disk pool + size.
	if req.DiskSizeGB > 0 || req.RootPool != "" {
		dArgs := []string{"config", "device", "override", req.Name, "root"}
		if req.RootPool != "" {
			dArgs = append(dArgs, "pool="+req.RootPool)
		}
		if req.DiskSizeGB > 0 {
			dArgs = append(dArgs, fmt.Sprintf("size=%dGB", req.DiskSizeGB))
		}
		log("Configuring root disk…")
		if out, err := exec.Command("lxc", dArgs...).CombinedOutput(); err != nil {
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
		if out, err := exec.Command("lxc", nArgs...).CombinedOutput(); err != nil {
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
		if out, err := exec.Command("lxc", dArgs...).CombinedOutput(); err != nil {
			log("WARNING: add device " + dev.DeviceName + ": " + strings.TrimSpace(string(out)))
		}
	}

	// FUSE: expose /dev/fuse inside the container.
	if req.FeatureFUSE {
		log("Adding FUSE device…")
		if out, err := exec.Command("lxc", "config", "device", "add", req.Name,
			"fuse", "unix-char", "path=/dev/fuse").CombinedOutput(); err != nil {
			log("WARNING: add fuse device: " + strings.TrimSpace(string(out)))
		}
	}

	needStart := req.AutoStart || req.RootPassword != "" || _hasStaticIPConfig(req.NICs)
	if needStart {
		log("Starting container…")
		if out, err := exec.Command("lxc", "start", req.Name).CombinedOutput(); err != nil {
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
		cmd := exec.Command("lxc", "exec", req.Name, "--", "chpasswd")
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
		cmd := exec.Command("lxc", "exec", req.Name, "--", "/bin/sh", "-c",
			"rm -f /etc/resolv.conf && cat > /etc/resolv.conf")
		cmd.Stdin = strings.NewReader(resolvConf)
		if out, err := cmd.CombinedOutput(); err != nil {
			log("WARNING: set DNS: " + strings.TrimSpace(string(out)))
		}
	}

	// Stop if we only started for post-init tasks and auto_start was not requested.
	if !req.AutoStart && needStart {
		log("Stopping container…")
		exec.Command("lxc", "stop", req.Name).Run()
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
		if exec.Command("lxc", "exec", name, "--", "true").Run() == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if time.Now().After(deadline) {
		return fmt.Errorf("exec not available after %d seconds", timeoutSec)
	}
	// Phase 2: wait for systemd to finish initialising (systemd-based containers).
	for time.Now().Before(deadline) {
		out, err := exec.Command("lxc", "exec", name, "--", "systemctl", "is-system-running").Output()
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
	cmd := exec.Command("lxc", "file", "push", "-", ctName+"/etc/systemd/network/"+devName+".network")
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
	cmd2 := exec.Command("lxc", "file", "push", "-", ctName+"/etc/network/interfaces.d/"+devName)
	cmd2.Stdin = strings.NewReader(ifd.String())
	cmd2.Run()
}

// _applyStaticIPCommands applies a static IP immediately via ip commands in a
// running container (the persistent config was already pushed before start).
func _applyStaticIPCommands(ctName, devName string, nic LXDNIC) {
	exec.Command("lxc", "exec", ctName, "--", "ip", "link", "set", devName, "up").Run()
	exec.Command("lxc", "exec", ctName, "--", "ip", "addr", "flush", "dev", devName).Run()
	exec.Command("lxc", "exec", ctName, "--", "ip", "addr", "add", nic.IPv4Addr, "dev", devName).Run()
	if nic.IPv4GW != "" {
		exec.Command("lxc", "exec", ctName, "--", "ip", "route", "replace", "default", "via", nic.IPv4GW).Run()
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
	out, err := exec.Command("lxc", "query", "/1.0/instances/"+name+"/snapshots?recursion=1").Output()
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
	if out, e := exec.Command("lxc", "config", "get", name, "limits.memory").Output(); e == nil {
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
	if out, e := exec.Command("lxc", "config", "device", "show", name).Output(); e == nil {
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
	if out, e := exec.Command("lxc", "query", "/1.0/storage-pools/"+poolName).Output(); e == nil {
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
	if out, err := exec.Command("lxc", "config", "device", "set", name, "root", "size.state="+sizeVal).CombinedOutput(); err != nil {
		if strings.Contains(string(out), "profile") {
			exec.Command("lxc", "config", "device", "override", name, "root", "size.state="+sizeVal).Run() //nolint:errcheck
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
		out, _ := exec.Command("lxc", "config", "get", name, "migration.stateful").Output()
		if strings.TrimSpace(string(out)) != "true" {
			// Try to set it. This fails when the VM is running — LXD requires the
			// VM to be stopped to change this key because it changes QEMU init args.
			if setOut, err := exec.Command("lxc", "config", "set", name, "migration.stateful", "true").CombinedOutput(); err != nil {
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

	args := []string{"snapshot", name}
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
	if out, err := exec.CommandContext(ctx, "lxc", args...).CombinedOutput(); err != nil {
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
	if out, err := exec.Command("lxc", "restore", name, snapName).CombinedOutput(); err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}

// CloneLXDFromSnapshot creates a new instance by copying from source/snapshot.
// The description is applied via a PATCH to the LXD API after creation.
func CloneLXDFromSnapshot(sourceName, snapName, newName, description string) error {
	src := sourceName + "/" + snapName
	if out, err := exec.Command("lxc", "copy", src, newName).CombinedOutput(); err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	if description != "" {
		body, _ := json.Marshal(map[string]string{"description": description})
		// Ignore errors — the clone already exists; description is cosmetic.
		exec.Command("lxc", "query", "-X", "PATCH",
			"/1.0/instances/"+newName, "-d", string(body)).Run()
	}
	return nil
}

// CloneLXDInstance copies an instance directly (no snapshot needed).
// The source may be running; LXD will take a live copy.
func CloneLXDInstance(sourceName, newName, description string) error {
	if out, err := exec.Command("lxc", "copy", sourceName, newName).CombinedOutput(); err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	if description != "" {
		body, _ := json.Marshal(map[string]string{"description": description})
		exec.Command("lxc", "query", "-X", "PATCH",
			"/1.0/instances/"+newName, "-d", string(body)).Run()
	}
	return nil
}

// DeleteLXDSnapshot deletes a single snapshot from the instance.
func DeleteLXDSnapshot(name, snapName string) error {
	if out, err := exec.Command("lxc", "delete", name+"/"+snapName).CombinedOutput(); err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
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
	raw, err := exec.Command("lxc", "query",
		"/1.0/instances/"+name+"/logs/lxc.log").Output()
	if err != nil {
		// Fall back to lxc info --show-log
		raw, err = exec.Command("lxc", "info", "--show-log", name).CombinedOutput()
		if err != nil {
			return nil, fmt.Errorf("lxc logs: %w", err)
		}
		// lxc info --show-log: skip header, start parsing after "Log:"
		return parseLXDLogAfterMarker(string(raw), "Log:"), nil
	}
	return parseLXDLogLines(string(raw)), nil
}

// GetLXDInstanceConsoleLog returns the contents of
// /var/log/lxd/<name>/console.log — the boot/console output the kernel
// + init system has written since the last start of a container.
//
// Two paths tried in order:
//
//  1. `lxc console <name> --show-log` over the LXD unix socket. Works
//     when the daemon's console buffer is populated. On LXD 5.0.x with
//     deb packaging the daemon often errors with "open : no such file
//     or directory" because of an LXD-internal path bug, so we fall
//     back to (2).
//  2. `sudo cat /var/log/lxd/<name>/console.log` — the file is root-
//     owned mode 0600 so direct read isn't possible from the zfsnas
//     service user. The ZFSNAS_LXD sudo alias grants either
//     `cat /var/log/lxd/*/console.log` (classic sudo, tight) or `cat *`
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
	if out, err := exec.Command("lxc", "console", name, "--show-log").CombinedOutput(); err == nil {
		s := string(out)
		if idx := strings.Index(s, "Console log:"); idx >= 0 {
			s = s[idx+len("Console log:"):]
		}
		return strings.TrimLeft(s, "\r\n"), nil
	}
	// 2. sudo cat fallback. Path is host-side, name comes from the
	//    instance list (regex-validated above) so no traversal risk.
	path := "/var/log/lxd/" + name + "/console.log"
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
	out, err := exec.Command("lxc", "move", name, "--storage", targetPool).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}
