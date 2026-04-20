package system

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

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
}

// LXDDisk is an extra virtual disk for a VM.
type LXDDisk struct {
	DeviceName string `json:"device_name"`
	Pool       string `json:"pool"`
	SizeGB     int    `json:"size_gb"`
}

// LXDNIC is a network interface for an instance.
type LXDNIC struct {
	DeviceName string `json:"device_name"`
	Network    string `json:"network"`
	MAC        string `json:"mac"`
	VlanID     int    `json:"vlan_id,omitempty"`
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
	NICs            []LXDNIC       `json:"nics"`
	USBDevices      []LXDUSBDevice `json:"usb_devices"`
	PCIDevices      []LXDPCIDevice `json:"pci_devices"`
	CloudInit       string         `json:"cloud_init"`
}

// LXDCreateContainerRequest contains all parameters for container creation.
type LXDCreateContainerRequest struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Image       string                 `json:"image"`
	Profile    string                 `json:"profile"`
	AutoStart  bool                   `json:"auto_start"`
	CPUCores   int                    `json:"cpu_cores"`
	MemoryMB   int                    `json:"memory_mb"`
	DiskSizeGB int                    `json:"disk_size_gb"`
	Devices    []LXDPassthroughDevice `json:"devices"`
	NICs       []LXDNIC               `json:"nics"`
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

	return LXDInstanceStats{
		Status:        raw2.Status,
		UptimeSec:     uptimeSec,
		CPUUsageNs:    raw2.CPU.Usage,
		CPUPct:        cpuPct,
		CPUCount:      cpuCount,
		MemUsedBytes:  raw2.Memory.Usage,
		MemPeakBytes:  raw2.Memory.UsagePeak,
		MemLimitBytes: memLimit,
		DiskUsedBytes: disk,
		DiskSizeBytes: diskSize,
		Processes:     raw2.Processes,
	}, nil
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
	return exec.Command("lxc", "start", name).Run()
}

// LXDStop stops a running instance gracefully (force=false) or immediately (force=true).
func LXDStop(name string, force bool) error {
	if !lxdNameRe.MatchString(name) {
		return fmt.Errorf("invalid instance name")
	}
	args := []string{"stop", name}
	if force {
		args = append(args, "--force")
	}
	return exec.Command("lxc", args...).Run()
}

// LXDRestart restarts a running instance.
func LXDRestart(name string) error {
	if !lxdNameRe.MatchString(name) {
		return fmt.Errorf("invalid instance name")
	}
	return exec.Command("lxc", "restart", name).Run()
}

// LXDDelete deletes an instance; if deleteVolumes is true, forces deletion including volumes.
func LXDDelete(name string, deleteVolumes bool) error {
	if !lxdNameRe.MatchString(name) {
		return fmt.Errorf("invalid instance name")
	}
	// Instance must be stopped first.
	_ = LXDStop(name, true)
	args := []string{"delete", name}
	if deleteVolumes {
		args = append(args, "--force")
	}
	return exec.Command("lxc", args...).Run()
}

// LXDNICConfig describes a network interface device on an instance.
type LXDNICConfig struct {
	Name        string `json:"name"`
	Bridge      string `json:"bridge"`              // "network" or "parent" value
	NICType     string `json:"nictype"`             // "network" or "bridged"
	VlanID      int    `json:"vlan_id,omitempty"`
	FromProfile bool   `json:"from_profile,omitempty"`
	CurrentIP   string `json:"current_ip,omitempty"` // live IPv4 from instance state
}

// LXDDiskConfig describes a disk device on an instance.
type LXDDiskConfig struct {
	Name        string `json:"name"`
	Pool        string `json:"pool,omitempty"`
	Size        string `json:"size,omitempty"`
	IsRoot      bool   `json:"is_root,omitempty"`
	FromProfile bool   `json:"from_profile,omitempty"`
	ZFSPath     string `json:"zfs_path,omitempty"`    // backing ZFS path
	ZFSType     string `json:"zfs_type,omitempty"`    // "zvol" | "dataset"
	CompRatio   string `json:"comp_ratio,omitempty"`  // e.g. "1.23x"
}

// LXDInstanceConfig holds the editable configuration of an LXD instance.
type LXDInstanceConfig struct {
	Description        string                 `json:"description"`
	CPULimit           string                 `json:"cpu_limit"`
	MemoryLimit        string                 `json:"memory_limit"`
	MemoryHugepages    bool                   `json:"memory_hugepages"`
	Autostart          bool                   `json:"autostart"`
	IsVM               bool                   `json:"is_vm"`
	NICs               []LXDNICConfig         `json:"nics"`
	Disks              []LXDDiskConfig        `json:"disks"`
	USBDevices         []LXDUSBDevice         `json:"usb_devices"`
	PCIDevices         []LXDPCIDevice         `json:"pci_devices"`
	PassthroughDevices []LXDPassthroughDevice `json:"passthrough_devices"`
}

var lxdDevNameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

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
	cfg := LXDInstanceConfig{
		Description:     raw.Description,
		CPULimit:        raw.Config["limits.cpu"],
		MemoryLimit:     raw.Config["limits.memory"],
		MemoryHugepages: raw.Config["limits.memory.hugepages"] == "true",
		Autostart:       raw.Config["boot.autostart"] == "true" || raw.Config["boot.autostart"] == "1",
		IsVM:            raw.Type == "virtual-machine",
	}
	for devName, devCfg := range raw.ExpandedDevices {
		_, isInstanceLevel := raw.Devices[devName]
		switch devCfg["type"] {
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
			bridge := devCfg["network"]
			nicType := "network"
			if bridge == "" {
				bridge = devCfg["parent"]
				nicType = "bridged"
			}
			vlanID := 0
			if v := devCfg["vlan"]; v != "" {
				fmt.Sscanf(v, "%d", &vlanID)
			}
			cfg.NICs = append(cfg.NICs, LXDNICConfig{
				Name:        devName,
				Bridge:      bridge,
				NICType:     nicType,
				VlanID:      vlanID,
				FromProfile: !isInstanceLevel,
			})
		case "disk":
			lxdPool := devCfg["pool"]
			isRoot := devCfg["path"] == "/"
			disk := LXDDiskConfig{
				Name:        devName,
				Pool:        lxdPool,
				Size:        devCfg["size"],
				IsRoot:      isRoot,
				FromProfile: !isInstanceLevel,
			}
			// For VMs with a known LXD storage pool, resolve the backing ZFS zvol.
			if lxdPool != "" {
				if zfsPool := lxdZFSPoolForLXDPool(lxdPool); zfsPool != "" {
					if raw.Type == "virtual-machine" {
						// LXD names the root-disk zvol "<vm>.block" under virtual-machines/
						zfsPath := zfsPool + "/virtual-machines/" + name + ".block"
						disk.ZFSPath = zfsPath
						disk.ZFSType = "zvol"
						disk.CompRatio = zfsGetCompRatio(zfsPath)
					} else {
						// Containers use a ZFS dataset under containers/
						zfsPath := zfsPool + "/containers/" + name
						disk.ZFSPath = zfsPath
						disk.ZFSType = "dataset"
						disk.CompRatio = zfsGetCompRatio(zfsPath)
					}
				}
			}
			cfg.Disks = append(cfg.Disks, disk)
		default:
			// For containers: capture any other device type as generic passthrough.
			if raw.Type != "virtual-machine" && isInstanceLevel {
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
			for i, nic := range cfg.NICs {
				if iface, ok := state.Network[nic.Name]; ok {
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

	return cfg, nil
}

// LXDSetConfig applies editable config and device changes to a named instance.
// cfg.NICs/Disks represent the desired instance-level device state; the backend diffs
// against current instance devices (not profile devices) to compute add/update/remove ops.
func LXDSetConfig(name string, cfg LXDInstanceConfig) error {
	if !lxdNameRe.MatchString(name) {
		return fmt.Errorf("invalid instance name")
	}

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
			out, err = exec.Command("lxc", "config", "set", name, key, val).CombinedOutput()
		}
		if err != nil {
			return fmt.Errorf("%s: %s", key, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := applyConf("limits.cpu", cfg.CPULimit); err != nil {
		return err
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
	autostart := "false"
	if cfg.Autostart {
		autostart = "true"
	}
	if err := applyConf("boot.autostart", autostart); err != nil {
		return err
	}

	// Fetch current instance-level devices for diff.
	rawOut, err := exec.Command("lxc", "query", "/1.0/instances/"+name).Output()
	if err != nil {
		return fmt.Errorf("query instance: %w", err)
	}
	var rawDev struct {
		Devices map[string]map[string]string `json:"devices"`
	}
	json.Unmarshal(rawOut, &rawDev)
	if rawDev.Devices == nil {
		rawDev.Devices = map[string]map[string]string{}
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
		default:
			curPassthrough[n] = d
		}
	}

	// ── NIC diff ──────────────────────────────────────────────────────────────
	wantNICs := map[string]struct{}{}
	for _, nic := range cfg.NICs {
		if !lxdDevNameRe.MatchString(nic.Name) {
			return fmt.Errorf("invalid NIC name: %s", nic.Name)
		}
		wantNICs[nic.Name] = struct{}{}
		cur, exists := curNICs[nic.Name]
		if !exists {
			// Add (also serves as instance-level override for profile devices).
			var args []string
			if nic.NICType == "network" {
				args = []string{"config", "device", "add", name, nic.Name, "nic", "network=" + nic.Bridge}
			} else {
				args = []string{"config", "device", "add", name, nic.Name, "nic", "nictype=bridged", "parent=" + nic.Bridge}
			}
			if nic.VlanID > 0 {
				args = append(args, fmt.Sprintf("vlan=%d", nic.VlanID))
			}
			if out, err := exec.Command("lxc", args...).CombinedOutput(); err != nil {
				return fmt.Errorf("add NIC %s: %s", nic.Name, strings.TrimSpace(string(out)))
			}
		} else {
			// Update bridge if changed.
			curBridge := cur["network"]
			bridgeKey := "network"
			if curBridge == "" {
				curBridge = cur["parent"]
				bridgeKey = "parent"
			}
			if nic.NICType == "bridged" {
				bridgeKey = "parent"
			}
			if curBridge != nic.Bridge {
				if out, err := exec.Command("lxc", "config", "device", "set", name, nic.Name, bridgeKey, nic.Bridge).CombinedOutput(); err != nil {
					return fmt.Errorf("update NIC %s: %s", nic.Name, strings.TrimSpace(string(out)))
				}
			}
			// Update VLAN if changed.
			curVlan := cur["vlan"]
			wantVlan := ""
			if nic.VlanID > 0 {
				wantVlan = fmt.Sprintf("%d", nic.VlanID)
			}
			if curVlan != wantVlan {
				if wantVlan == "" {
					exec.Command("lxc", "config", "device", "unset", name, nic.Name, "vlan").Run()
				} else {
					if out, err := exec.Command("lxc", "config", "device", "set", name, nic.Name, "vlan", wantVlan).CombinedOutput(); err != nil {
						return fmt.Errorf("update NIC %s vlan: %s", nic.Name, strings.TrimSpace(string(out)))
					}
				}
			}
		}
	}
	for n := range curNICs {
		if _, ok := wantNICs[n]; !ok {
			if out, err := exec.Command("lxc", "config", "device", "remove", name, n).CombinedOutput(); err != nil {
				return fmt.Errorf("remove NIC %s: %s", n, strings.TrimSpace(string(out)))
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
			args := []string{"config", "device", "add", name, disk.Name, "disk", "pool=" + disk.Pool}
			if disk.Size != "" {
				args = append(args, "size="+disk.Size)
			}
			if out, err := exec.Command("lxc", args...).CombinedOutput(); err != nil {
				return fmt.Errorf("add disk %s: %s", disk.Name, strings.TrimSpace(string(out)))
			}
		} else if disk.Size != "" && cur["size"] != disk.Size {
			if out, err := exec.Command("lxc", "config", "device", "set", name, disk.Name, "size", disk.Size).CombinedOutput(); err != nil {
				return fmt.Errorf("resize disk %s: %s", disk.Name, strings.TrimSpace(string(out)))
			}
		}
	}
	for n, d := range curDisks {
		if d["path"] == "/" {
			continue // never auto-remove root disk
		}
		if _, ok := wantDisks[n]; !ok {
			if out, err := exec.Command("lxc", "config", "device", "remove", name, n).CombinedOutput(); err != nil {
				return fmt.Errorf("remove disk %s: %s", n, strings.TrimSpace(string(out)))
			}
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

// LXDListNetworks lists LXD bridge network names.
func LXDListNetworks() ([]string, error) {
	out, err := exec.Command("lxc", "network", "list", "--format", "json").Output()
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	var names []string
	for _, r := range raw {
		if r.Type == "bridge" {
			names = append(names, r.Name)
		}
	}
	return names, nil
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

	image := req.Image
	if !strings.Contains(image, ":") {
		image = "images:" + image
	}

	args := []string{"init", image, req.Name, "--vm", "-p", profile}
	if req.VCPU > 0 {
		args = append(args, "-c", fmt.Sprintf("limits.cpu=%d", req.VCPU))
	}
	if req.MemoryMB > 0 {
		args = append(args, "-c", fmt.Sprintf("limits.memory=%dMB", req.MemoryMB))
	}
	if req.MemoryHugepages {
		args = append(args, "-c", "limits.memory.hugepages=true")
	}
	if req.AutoStart {
		args = append(args, "-c", "boot.autostart=true")
	}
	if req.CloudInit != "" {
		args = append(args, "-c", "user.user-data="+req.CloudInit)
	}

	log("Initialising VM " + req.Name + "…")
	if out, err := exec.Command("lxc", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("lxc init: %s: %w", strings.TrimSpace(string(out)), err)
	}

	// Set description (display name).
	if req.Description != "" {
		descJSON, _ := json.Marshal(req.Description)
		exec.Command("lxc", "query", "-X", "PATCH", "/1.0/instances/"+req.Name,
			"--data", fmt.Sprintf(`{"description":%s}`, descJSON)).Run()
	}

	// Override root disk pool/size.
	if req.RootPool != "" || req.RootSizeGB > 0 {
		dArgs := []string{"config", "device", "override", req.Name, "root"}
		if req.RootPool != "" {
			dArgs = append(dArgs, "pool="+req.RootPool)
		}
		if req.RootSizeGB > 0 {
			dArgs = append(dArgs, fmt.Sprintf("size=%dGB", req.RootSizeGB))
		}
		log("Configuring root disk…")
		if out, err := exec.Command("lxc", dArgs...).CombinedOutput(); err != nil {
			log("WARNING: root disk config: " + strings.TrimSpace(string(out)))
		}
	}

	// Add extra disks.
	for i, disk := range req.ExtraDisks {
		devName := disk.DeviceName
		if devName == "" {
			devName = fmt.Sprintf("disk%d", i+1)
		}
		dArgs := []string{"config", "device", "add", req.Name, devName, "disk",
			"pool=" + disk.Pool,
			fmt.Sprintf("size=%dGB", disk.SizeGB),
		}
		log("Adding disk " + devName + "…")
		if out, err := exec.Command("lxc", dArgs...).CombinedOutput(); err != nil {
			log("WARNING: add disk " + devName + ": " + strings.TrimSpace(string(out)))
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
	if req.MemoryMB > 0 {
		args = append(args, "-c", fmt.Sprintf("limits.memory=%dMB", req.MemoryMB))
	}
	if req.AutoStart {
		args = append(args, "-c", "boot.autostart=true")
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

	// Root disk size.
	if req.DiskSizeGB > 0 {
		dArgs := []string{"config", "device", "override", req.Name, "root",
			fmt.Sprintf("size=%dGB", req.DiskSizeGB),
		}
		log("Setting root disk size…")
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
		log("Adding NIC " + devName + "…")
		if out, err := exec.Command("lxc", nArgs...).CombinedOutput(); err != nil {
			log("WARNING: add NIC " + devName + ": " + strings.TrimSpace(string(out)))
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

	if req.AutoStart {
		log("Starting container…")
		if out, err := exec.Command("lxc", "start", req.Name).CombinedOutput(); err != nil {
			return fmt.Errorf("lxc start: %s: %w", strings.TrimSpace(string(out)), err)
		}
	}

	log("Done.")
	return nil
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
