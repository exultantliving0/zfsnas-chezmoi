package system

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// lxdBinaryPath returns the absolute path to the lxd binary,
// checking /usr/bin (Debian) then /usr/sbin (Ubuntu).
func lxdBinaryPath() string {
	for _, p := range []string{"/usr/bin/lxd", "/usr/sbin/lxd"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "lxd" // fallback to PATH
}

// LXDEnablePrereqResult is returned by LXDEnableCheckPrereqs.
type LXDEnablePrereqResult struct {
	SudoersOK   bool     `json:"sudoers_ok"`
	NetworkOK   bool     `json:"network_ok"`
	IsDebian    bool     `json:"is_debian"`
	HasPools    bool     `json:"has_pools"`
	AllOK       bool     `json:"all_ok"`
	SudoersNote string   `json:"sudoers_note,omitempty"`
	NetworkNote string   `json:"network_note,omitempty"`
	OSNote      string   `json:"os_note,omitempty"`
	PoolsNote   string   `json:"pools_note,omitempty"`
	ZFSPools    []string `json:"zfs_pools"`
}

// LXDEnableStepStatus tracks one step of the enablement job.
type LXDEnableStepStatus struct {
	ID     int    `json:"id"`
	Label  string `json:"label"`
	Status string `json:"status"` // "pending" | "running" | "done" | "error"
	Error  string `json:"error,omitempty"`
}

// LXDEnableJob tracks a running LXD-enable operation.
type LXDEnableJob struct {
	mu     sync.Mutex
	Status string                `json:"status"` // "running" | "done" | "error"
	Error  string                `json:"error,omitempty"`
	Steps  []LXDEnableStepStatus `json:"steps"`
	Lines  []string              `json:"lines"`
	cancel context.CancelFunc
}

func (j *LXDEnableJob) log(line string) {
	j.mu.Lock()
	j.Lines = append(j.Lines, line)
	j.mu.Unlock()
}

func (j *LXDEnableJob) setStep(id int, status, errMsg string) {
	j.mu.Lock()
	for i := range j.Steps {
		if j.Steps[i].ID == id {
			j.Steps[i].Status = status
			j.Steps[i].Error = errMsg
			break
		}
	}
	j.mu.Unlock()
}

func (j *LXDEnableJob) Cancel() {
	if j.cancel != nil {
		j.cancel()
	}
}

func (j *LXDEnableJob) Snapshot() LXDEnableJob {
	j.mu.Lock()
	defer j.mu.Unlock()
	steps := make([]LXDEnableStepStatus, len(j.Steps))
	copy(steps, j.Steps)
	lines := make([]string, len(j.Lines))
	copy(lines, j.Lines)
	return LXDEnableJob{Status: j.Status, Error: j.Error, Steps: steps, Lines: lines}
}

// LXDEnableCheckPrereqs validates all four prerequisites for enabling VMs & Containers.
func LXDEnableCheckPrereqs() LXDEnablePrereqResult {
	res := LXDEnablePrereqResult{}

	// 1. Sudoers: can we run apt-get? (root, all, or hardened with ZFSNAS_APT)
	sudo := CheckSudoAccess()
	switch sudo.Type {
	case "root", "all":
		res.SudoersOK = true
	case "hardened":
		// Check that ZFSNAS_APT (apt-get *) is present
		missing := false
		for _, m := range sudo.MissingCommands {
			if strings.Contains(m, "apt-get") {
				missing = true
				break
			}
		}
		if missing {
			res.SudoersNote = "Hardened sudoers is missing apt-get commands (ZFSNAS_APT). Apply the full sudoers template from the Security tab first."
		} else {
			res.SudoersOK = true
		}
	default:
		res.SudoersNote = "No sudo access detected. The service account needs passwordless sudo to install packages."
	}

	// 2. Network: /etc/network/interfaces exists and no netplan files present
	if _, err := os.Stat("/etc/network/interfaces"); err == nil {
		// Check for netplan files
		netplanFiles, _ := filepath.Glob("/etc/netplan/*.yaml")
		if len(netplanFiles) > 0 {
			res.NetworkNote = "Netplan configuration detected (/etc/netplan/*.yaml). This feature requires /etc/network/interfaces (ifupdown)."
		} else {
			res.NetworkOK = true
		}
	} else {
		res.NetworkNote = "/etc/network/interfaces not found. This feature requires the ifupdown networking system."
	}

	// 3. OS is Debian (not Ubuntu or other)
	osRelease := readOSRelease()
	if id, ok := osRelease["ID"]; ok && strings.EqualFold(id, "debian") {
		res.IsDebian = true
	} else {
		name := osRelease["NAME"]
		if name == "" {
			name = osRelease["ID"]
		}
		if name == "" {
			name = "unknown OS"
		}
		res.OSNote = fmt.Sprintf("Detected: %s. This feature requires Debian Linux.", name)
	}

	// 4. At least one ZFS pool
	pools, err := GetAllPools()
	if err == nil && len(pools) > 0 {
		res.HasPools = true
		for _, p := range pools {
			res.ZFSPools = append(res.ZFSPools, p.Name)
		}
	} else {
		res.PoolsNote = "No ZFS storage pools found. Create a pool first before enabling VMs & Containers."
	}

	res.AllOK = res.SudoersOK && res.NetworkOK && res.IsDebian && res.HasPools
	return res
}

func readOSRelease() map[string]string {
	m := map[string]string{}
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return m
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		idx := strings.IndexByte(line, '=')
		if idx < 0 {
			continue
		}
		k := line[:idx]
		v := strings.Trim(line[idx+1:], `"'`)
		m[k] = v
	}
	return m
}

// NewLXDEnableJob creates a fresh job with the four steps pre-populated.
func NewLXDEnableJob(cancel context.CancelFunc) *LXDEnableJob {
	return &LXDEnableJob{
		Status: "running",
		cancel: cancel,
		Steps: []LXDEnableStepStatus{
			{ID: 1, Label: "Install LXD and QEMU packages", Status: "pending"},
			{ID: 2, Label: "Initialise LXD", Status: "pending"},
			{ID: 3, Label: "Configure physical NIC bridges", Status: "pending"},
			{ID: 4, Label: "Install chrony (time synchronisation)", Status: "pending"},
		},
	}
}

// LXDEnableFeature runs all four enablement steps and updates job state throughout.
// It is designed to be called in a goroutine.
func LXDEnableFeature(ctx context.Context, storagePool string, job *LXDEnableJob) {
	done := func(err error) {
		job.mu.Lock()
		if err != nil {
			job.Status = "error"
			job.Error = err.Error()
		} else {
			job.Status = "done"
		}
		job.mu.Unlock()
	}

	// Step 1 — Install packages
	if err := lxdStep1Packages(ctx, job); err != nil {
		done(err)
		return
	}

	// Step 2 — Initialise LXD
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "znas"
	}
	if err := lxdStep2Init(ctx, storagePool, hostname, job); err != nil {
		done(err)
		return
	}

	// Step 3 — Configure NIC bridges
	if err := lxdStep3Bridges(ctx, job); err != nil {
		done(err)
		return
	}

	// Step 4 — Chrony
	if err := lxdStep4Chrony(ctx, job); err != nil {
		done(err)
		return
	}

	done(nil)
}

// runCmdLog runs a sudo command, streaming each output line into job.
func runCmdLog(ctx context.Context, job *LXDEnableJob, name string, args ...string) error {
	full := append([]string{"sudo"}, append([]string{name}, args...)...)
	job.log("$ " + strings.Join(full, " "))
	cmd := exec.CommandContext(ctx, "sudo", append([]string{name}, args...)...)
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		pw.Close()
		pr.Close()
		return err
	}
	pw.Close()
	scanner := bufio.NewScanner(pr)
	for scanner.Scan() {
		job.log(scanner.Text())
	}
	pr.Close()
	return cmd.Wait()
}

// ── Step 1: packages ──────────────────────────────────────────────────────────

func lxdStep1Packages(ctx context.Context, job *LXDEnableJob) error {
	job.setStep(1, "running", "")
	pkgs := []string{
		"lxd", "lxd-client", "lxd-agent",
		"bridge-utils",
		"qemu-system-x86", "qemu-kvm",
		"dnsmasq-base",
		"swtpm",    // required for VMs with virtual TPM devices
		"ovmf",     // UEFI firmware for x86 VMs (OVMF_CODE.4MB.fd)
		"sshpass",  // required for InterLink push operations
	}
	job.log("Running apt-get update…")
	if err := runCmdLog(ctx, job, "/usr/bin/apt-get", "update"); err != nil {
		job.setStep(1, "error", "apt-get update failed: "+err.Error())
		return fmt.Errorf("apt-get update: %w", err)
	}
	job.log("Installing packages: " + strings.Join(pkgs, " "))
	args := append([]string{"install", "-y"}, pkgs...)
	if err := runCmdLog(ctx, job, "/usr/bin/apt-get", args...); err != nil {
		job.setStep(1, "error", "package install failed: "+err.Error())
		return fmt.Errorf("apt-get install: %w", err)
	}

	// Create cross-distro OVMF symlinks (Ubuntu ↔ Debian naming difference).
	job.log("Creating OVMF firmware compatibility symlinks…")
	EnsureOVMFCompat()

	job.setStep(1, "done", "")
	return nil
}

// EnsureOVMFCompat creates bidirectional symlinks between Ubuntu and Debian
// OVMF firmware file names so VMs can start on either distro after a push.
//
//   Ubuntu: /usr/share/OVMF/OVMF_CODE.4MB.fd
//   Debian: /usr/share/OVMF/OVMF_CODE_4M.fd
func EnsureOVMFCompat() {
	pairs := [][2]string{
		{"/usr/share/OVMF/OVMF_CODE.4MB.fd", "/usr/share/OVMF/OVMF_CODE_4M.fd"},
		{"/usr/share/OVMF/OVMF_VARS.4MB.fd", "/usr/share/OVMF/OVMF_VARS_4M.fd"},
	}
	for _, p := range pairs {
		a, b := p[0], p[1]
		aOK := fileExists(a)
		bOK := fileExists(b)
		switch {
		case aOK && !bOK:
			exec.Command("sudo", "/usr/bin/ln", "-sf", a, b).Run() //nolint:errcheck
		case bOK && !aOK:
			exec.Command("sudo", "/usr/bin/ln", "-sf", b, a).Run() //nolint:errcheck
		}
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// ── Step 2: lxd init ─────────────────────────────────────────────────────────

// lxdIsInitialized returns true when LXD already has at least one storage pool,
// meaning a previous lxd init (or manual setup) was done.
func lxdIsInitialized() bool {
	out, err := exec.Command("lxc", "storage", "list", "--format", "csv").Output()
	return err == nil && len(strings.TrimSpace(string(out))) > 0
}

// runLXCLog runs a lxc command (no sudo) and streams its output into the job log.
func runLXCLog(ctx context.Context, job *LXDEnableJob, args ...string) error {
	job.log("$ lxc " + strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, "lxc", args...)
	out, err := cmd.CombinedOutput()
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			job.log(line)
		}
	}
	return err
}

func lxdStep2Init(ctx context.Context, storagePool, hostname string, job *LXDEnableJob) error {
	job.setStep(2, "running", "")

	// Add the service account to the lxd group first (non-fatal — may already be done).
	job.log("Ensuring service account is in the lxd group…")
	if err := runCmdLog(ctx, job, "/usr/sbin/usermod", "-a", "-G", "lxd", "zfsnas"); err != nil {
		job.log("Warning: usermod returned error (may already be in group): " + err.Error())
	}

	var err error
	if lxdIsInitialized() {
		job.log("LXD is already initialised — applying incremental configuration…")
		err = lxdConfigureExisting(ctx, storagePool, hostname, job)
	} else {
		err = lxdInitFreshPreseed(ctx, storagePool, hostname, job)
	}
	if err != nil {
		return err
	}

	// Generate lxc client cert for cross-server VM push (idempotent).
	job.log("Generating lxc client certificate for VM push…")
	if certErr := LXDEnsureClientCert(); certErr != nil {
		job.log("Warning: could not generate lxc client cert: " + certErr.Error())
	} else {
		job.log("LXD client certificate ready.")
	}
	return nil
}

// lxdInitFreshPreseed runs `sudo lxd init --preseed` for a brand-new LXD installation.
func lxdInitFreshPreseed(ctx context.Context, storagePool, hostname string, job *LXDEnableJob) error {
	dataset := storagePool + "/LXD-" + hostname
	preseed := fmt.Sprintf(`config:
  core.https_address: ':8444'
networks:
- name: host-nat
  type: bridge
  config:
    ipv4.address: auto
    ipv6.address: none
storage_pools:
- name: default
  driver: zfs
  config:
    source: %s
profiles:
- name: default
  config: {}
  description: ""
  devices:
    eth0:
      name: eth0
      network: host-nat
      type: nic
    root:
      path: /
      pool: default
      type: disk
projects:
- config:
    features.images: "true"
    features.networks: "true"
    features.profiles: "true"
    features.storage.volumes: "true"
  description: Default LXD project
  name: default
`, dataset)

	lxdBin := lxdBinaryPath()
	job.log("Running lxd init --preseed (" + lxdBin + ")…")
	cmd := exec.CommandContext(ctx, "sudo", lxdBin, "init", "--preseed")
	cmd.Stdin = strings.NewReader(preseed)
	out, err := cmd.CombinedOutput()
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			job.log(line)
		}
	}
	if err != nil {
		job.setStep(2, "error", "lxd init failed: "+err.Error())
		return fmt.Errorf("lxd init: %w", err)
	}
	job.setStep(2, "done", "")
	return nil
}

// lxdConfigureExisting applies incremental configuration to an already-initialised LXD:
// sets core.https_address and ensures the host-nat bridge exists.
func lxdConfigureExisting(ctx context.Context, storagePool, hostname string, job *LXDEnableJob) error {
	// Set API listen address
	job.log("Setting core.https_address to :8444…")
	if err := runLXCLog(ctx, job, "config", "set", "core.https_address", ":8444"); err != nil {
		job.log("Warning: could not set core.https_address: " + err.Error())
	}

	// Ensure host-nat bridge exists
	out, _ := exec.Command("lxc", "network", "show", "host-nat").Output()
	if len(strings.TrimSpace(string(out))) == 0 {
		job.log("Creating host-nat bridge…")
		if err := runLXCLog(ctx, job, "network", "create", "host-nat",
			"ipv4.address=auto", "ipv6.address=none"); err != nil {
			job.log("Warning: could not create host-nat bridge: " + err.Error())
		}
	} else {
		job.log("host-nat bridge already exists.")
	}

	// Ensure default profile uses host-nat for eth0
	job.log("Updating default profile to use host-nat…")
	if err := runLXCLog(ctx, job, "profile", "device", "add", "default", "eth0", "nic",
		"nictype=bridged", "parent=host-nat", "name=eth0"); err != nil {
		// device may already exist — try set instead
		if err2 := runLXCLog(ctx, job, "profile", "device", "set", "default", "eth0", "network=host-nat"); err2 != nil {
			job.log("Warning: could not update default profile eth0: " + err2.Error())
		}
	}
	// Ensure default profile has root disk
	if err := runLXCLog(ctx, job, "profile", "device", "add", "default", "root", "disk",
		"path=/", "pool=default"); err != nil {
		job.log("Note: root disk device may already exist in default profile.")
	}

	// Ensure default storage pool exists pointing at chosen ZFS pool
	poolOut, _ := exec.Command("lxc", "storage", "show", "default").Output()
	if len(strings.TrimSpace(string(poolOut))) == 0 {
		dataset := storagePool + "/LXD-" + hostname
		job.log("Creating default storage pool on " + dataset + "…")
		if err := runLXCLog(ctx, job, "storage", "create", "default", "zfs",
			"source="+dataset); err != nil {
			job.setStep(2, "error", "create storage pool: "+err.Error())
			return fmt.Errorf("create storage pool: %w", err)
		}
	} else {
		job.log("Default storage pool already exists.")
	}

	job.setStep(2, "done", "")
	return nil
}

// ── Step 3: network bridges ───────────────────────────────────────────────────

type bridgeCandidate struct {
	NIC     string
	IP      string
	Prefix  int
	Gateway string
	Bridge  string
}

func lxdStep3Bridges(ctx context.Context, job *LXDEnableJob) error {
	job.setStep(3, "running", "")

	candidates, err := detectBridgeCandidates()
	if err != nil {
		job.setStep(3, "error", "detect NICs: "+err.Error())
		return fmt.Errorf("detect bridge candidates: %w", err)
	}
	if len(candidates) == 0 {
		job.log("No unconfigured physical NICs with IPv4 addresses found; skipping bridge setup.")
		job.setStep(3, "done", "")
		return nil
	}

	for _, c := range candidates {
		job.log(fmt.Sprintf("Will bridge %s → %s (%s/%d gw %s)", c.NIC, c.Bridge, c.IP, c.Prefix, c.Gateway))
	}

	// /etc/network/interfaces is edited in-process; this requires the portal to
	// run as root (no sudo entry is granted for this path).
	if os.Getuid() != 0 {
		job.setStep(3, "error", "writing /etc/network/interfaces requires running as root")
		return fmt.Errorf("writing /etc/network/interfaces requires running as root")
	}

	// Backup existing interfaces file
	backupPath := "/etc/network/interfaces.bak"
	job.log("Backing up /etc/network/interfaces to " + backupPath)
	content, err := os.ReadFile("/etc/network/interfaces")
	if err != nil {
		job.setStep(3, "error", "read interfaces: "+err.Error())
		return fmt.Errorf("read /etc/network/interfaces: %w", err)
	}
	if err := os.WriteFile(backupPath, content, 0644); err != nil {
		job.log("Warning: backup failed: " + err.Error())
	}

	newContent := rewriteInterfacesForBridges(string(content), candidates)

	job.log("Writing new /etc/network/interfaces…")
	if err := os.WriteFile("/etc/network/interfaces", []byte(newContent), 0644); err != nil {
		job.setStep(3, "error", "write interfaces: "+err.Error())
		return fmt.Errorf("write /etc/network/interfaces: %w", err)
	}

	// Restart networking
	job.log("Restarting networking (your connection may briefly drop)…")
	if err := runCmdLog(ctx, job, "/usr/bin/systemctl", "restart", "networking"); err != nil {
		job.setStep(3, "error", "restart networking: "+err.Error())
		return fmt.Errorf("restart networking: %w", err)
	}

	job.setStep(3, "done", "")
	return nil
}

// ipAddrIface is a minimal parse of one entry from `ip -j addr`.
type ipAddrIface struct {
	IfName   string `json:"ifname"`
	LinkType string `json:"link_type"`
	Flags    []string `json:"flags"`
	Master   string `json:"master"`
	AddrInfo []struct {
		Family    string `json:"family"`
		Local     string `json:"local"`
		PrefixLen int    `json:"prefixlen"`
	} `json:"addr_info"`
}

// ipRouteEntry is a minimal parse of one entry from `ip -j route`.
type ipRouteEntry struct {
	Dst     string `json:"dst"`
	Gateway string `json:"gateway,omitempty"`
	Dev     string `json:"dev"`
}

// virtualIfRe matches names that are clearly not physical NICs.
var virtualIfRe = regexp.MustCompile(`^(vmbr|lxdbr|br-|virbr|docker|tap|tun|dummy|lo|bond|team)`)

func detectBridgeCandidates() ([]bridgeCandidate, error) {
	// Get all interfaces
	addrOut, err := exec.Command("ip", "-j", "addr").Output()
	if err != nil {
		return nil, fmt.Errorf("ip -j addr: %w", err)
	}
	var ifaces []ipAddrIface
	if err := json.Unmarshal(addrOut, &ifaces); err != nil {
		return nil, fmt.Errorf("parse ip addr: %w", err)
	}

	// Find default gateway and which interface it's on
	routeOut, _ := exec.Command("ip", "-j", "route").Output()
	var routes []ipRouteEntry
	_ = json.Unmarshal(routeOut, &routes)
	gwByDev := map[string]string{}
	for _, r := range routes {
		if r.Dst == "default" && r.Gateway != "" {
			if _, exists := gwByDev[r.Dev]; !exists {
				gwByDev[r.Dev] = r.Gateway
			}
		}
	}

	var candidates []bridgeCandidate
	bridgeIdx := 0
	for _, iface := range ifaces {
		if iface.LinkType != "ether" {
			continue
		}
		if virtualIfRe.MatchString(iface.IfName) {
			continue
		}
		if strings.Contains(iface.IfName, ".") {
			continue // VLAN sub-interface
		}
		if iface.Master != "" {
			continue // already enslaved to a bridge
		}
		// Must be a physical device (has /sys/class/net/<name>/device symlink)
		if _, err := os.Stat("/sys/class/net/" + iface.IfName + "/device"); err != nil {
			continue
		}
		// Must already be a bridge member or have an IP we can move
		// Find IPv4 address
		ip := ""
		prefix := 0
		for _, a := range iface.AddrInfo {
			if a.Family == "inet" {
				ip = a.Local
				prefix = a.PrefixLen
				break
			}
		}
		if ip == "" {
			continue
		}
		bridge := fmt.Sprintf("vmbr%d", bridgeIdx)
		bridgeIdx++
		candidates = append(candidates, bridgeCandidate{
			NIC:     iface.IfName,
			IP:      ip,
			Prefix:  prefix,
			Gateway: gwByDev[iface.IfName],
			Bridge:  bridge,
		})
	}
	return candidates, nil
}

// rewriteInterfacesForBridges removes existing NIC stanzas and appends
// a manual NIC stanza + bridge stanza for each candidate, preserving
// dns-nameservers, dns-search, and mtu from the original NIC stanza.
func rewriteInterfacesForBridges(content string, candidates []bridgeCandidate) string {
	nicSet := map[string]bool{}
	for _, c := range candidates {
		nicSet[c.NIC] = true
	}

	// Per-NIC preserved settings extracted from stripped stanzas.
	type nicMeta struct {
		dns  string // dns-nameservers value
		search string // dns-search value
		mtu  string // mtu value
	}
	nicMetas := map[string]*nicMeta{}
	for _, c := range candidates {
		nicMetas[c.NIC] = &nicMeta{}
	}

	lines := strings.Split(content, "\n")
	var kept []string
	inSkip := false
	currentNIC := ""

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Detect start of a stanza we want to remove
		if !inSkip {
			fields := strings.Fields(trimmed)
			if len(fields) >= 2 {
				kw := fields[0]
				name := fields[1]
				if (kw == "auto" || kw == "allow-hotplug" || kw == "iface" || kw == "mapping") && nicSet[name] {
					inSkip = true
					currentNIC = name
					continue
				}
			}
		}

		if inSkip {
			// Collect preserved settings from the skipped stanza
			if meta, ok := nicMetas[currentNIC]; ok && (strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")) {
				fields := strings.Fields(trimmed)
				if len(fields) >= 2 {
					switch fields[0] {
					case "dns-nameservers":
						meta.dns = strings.Join(fields[1:], " ")
					case "dns-search":
						meta.search = strings.Join(fields[1:], " ")
					case "mtu":
						meta.mtu = fields[1]
					}
				}
			}

			if trimmed == "" {
				inSkip = false
				currentNIC = ""
				kept = append(kept, line)
			} else if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
				continue
			} else {
				inSkip = false
				currentNIC = ""
				fields := strings.Fields(trimmed)
				if len(fields) >= 2 {
					kw := fields[0]
					name := fields[1]
					if (kw == "auto" || kw == "allow-hotplug" || kw == "iface" || kw == "mapping") && nicSet[name] {
						inSkip = true
						currentNIC = name
						continue
					}
				}
				kept = append(kept, line)
			}
			continue
		}
		kept = append(kept, line)
	}

	result := strings.TrimRight(strings.Join(kept, "\n"), "\n") + "\n"

	for _, c := range candidates {
		meta := nicMetas[c.NIC]
		nicBlock := fmt.Sprintf("\nauto %s\niface %s inet manual\n", c.NIC, c.NIC)
		if meta != nil && meta.mtu != "" {
			nicBlock += fmt.Sprintf("    mtu %s\n", meta.mtu)
		}
		bridgeBlock := fmt.Sprintf("\nauto %s\niface %s inet static\n    address %s/%d\n",
			c.Bridge, c.Bridge, c.IP, c.Prefix)
		if c.Gateway != "" {
			bridgeBlock += fmt.Sprintf("    gateway %s\n", c.Gateway)
		}
		if meta != nil && meta.dns != "" {
			bridgeBlock += fmt.Sprintf("    dns-nameservers %s\n", meta.dns)
		}
		if meta != nil && meta.search != "" {
			bridgeBlock += fmt.Sprintf("    dns-search %s\n", meta.search)
		}
		if meta != nil && meta.mtu != "" {
			bridgeBlock += fmt.Sprintf("    mtu %s\n", meta.mtu)
		}
		bridgeBlock += fmt.Sprintf("    bridge_ports %s\n    bridge_stp off\n    bridge_fd 0\n    bridge-vlan-aware yes\n    bridge-vids 2-4094\n", c.NIC)
		result += nicBlock + bridgeBlock
	}
	return result
}

// ── Step 4: chrony ────────────────────────────────────────────────────────────

func lxdStep4Chrony(ctx context.Context, job *LXDEnableJob) error {
	job.setStep(4, "running", "")
	// Check if chrony is already installed
	if _, err := exec.LookPath("chronyc"); err == nil {
		job.log("chrony is already installed; skipping.")
		job.setStep(4, "done", "")
		return nil
	}
	job.log("Installing chrony…")
	if err := runCmdLog(ctx, job, "/usr/bin/apt-get", "install", "-y", "chrony"); err != nil {
		job.setStep(4, "error", "chrony install failed: "+err.Error())
		return fmt.Errorf("install chrony: %w", err)
	}
	job.setStep(4, "done", "")
	return nil
}
