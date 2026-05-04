package system

// Memory compression (zram-tools) backend.
//
// Single source of truth lives in three OS surfaces:
//   • /etc/default/zramswap        — the persistent config (PERCENT, ALGO)
//   • systemctl is-active zramswap — runtime on/off
//   • /sys/block/zram0/* + /proc/swaps — live counters
//
// We never persist state in ZNAS's config.json: that would just drift
// from the OS the moment an admin runs `dpkg-reconfigure zram-tools` or
// edits the config file by hand.

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

const (
	memCompConfigPath = "/etc/default/zramswap"
	memCompService    = "zramswap"
	memCompPackage    = "zram-tools"
)

// MemCompStatus is what GET /api/memcomp/status returns and what the
// MemProcs snapshot embeds for the topbar gauge / dashboard chart.
type MemCompStatus struct {
	Available      bool    `json:"available"`        // zram-tools installed
	Enabled        bool    `json:"enabled"`          // service active + device backed
	PercentRAM     int     `json:"percent_ram"`      // PERCENT= in config (0-100)
	Algorithm      string  `json:"algorithm"`        // ALGO= in config (zstd/lz4/lzo)
	DiskSizeBytes  int64   `json:"disk_size_bytes"`  // /sys/block/zram0/disksize  (configured cap)
	OrigDataBytes  int64   `json:"orig_data_bytes"`  // uncompressed bytes held in zram
	ComprDataBytes int64   `json:"compr_data_bytes"` // compressed bytes physically in RAM
	MemUsedBytes   int64   `json:"mem_used_bytes"`   // total RAM the zram allocator owns (incl. overhead)
	Ratio          float64 `json:"ratio"`            // OrigDataBytes / max(ComprDataBytes, 1)
	SwapUsedBytes  int64   `json:"swap_used_bytes"`  // bytes from /proc/swaps for /dev/zram0
}

// MemCompConfig is the editable surface the UI sends to PUT /api/memcomp/config.
// PercentRAM range is enforced server-side: 5..75. We refuse 0 (use Enabled=false
// instead) and >75 (silly value, would crash the host).
type MemCompConfig struct {
	Enabled    bool   `json:"enabled"`
	PercentRAM int    `json:"percent_ram"`
	Algorithm  string `json:"algorithm"`
}

// MemCompPrereqsInstalled reports whether the zram-tools package is present.
// `dpkg-query -W -f='${Status}'` returns "install ok installed" for installed
// packages; anything else (including the package being unknown) means missing.
func MemCompPrereqsInstalled() bool {
	out, err := exec.Command("dpkg-query", "-W", "-f=${Status}", memCompPackage).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "install ok installed")
}

// GetMemCompStatus returns a fully-populated status snapshot. Safe to call
// when the package isn't installed (returns Available=false, zeros elsewhere).
func GetMemCompStatus() MemCompStatus {
	s := MemCompStatus{
		Available: MemCompPrereqsInstalled(),
		Algorithm: "zstd",
	}
	// Always read /etc/default/zramswap if it exists; the package writes
	// a default file on install even before we touch it.
	if pct, algo, ok := readMemCompConfigFile(); ok {
		s.PercentRAM = pct
		if algo != "" {
			s.Algorithm = algo
		}
	}
	if s.Available {
		// "Enabled" = the device is actually live, not just whether the
		// systemd unit reports active. zramswap.service is `Type=oneshot`
		// and stays in either "inactive (dead)" or "failed" once its setup
		// script returns — neither of which prevents /dev/zram0 from
		// continuing to serve as a swap device. We've also seen prod
		// hosts where the device was set up by systemd-zram-generator
		// rather than zram-tools, in which case zramswap.service is
		// permanently inactive but the device works fine. So we trust
		// the disksize file: if /sys/block/zram0/disksize > 0 AND the
		// device shows up in /proc/swaps, it's serving as swap.
		s.Enabled = readSysFileInt64("/sys/block/zram0/disksize") > 0 &&
			readZramSwapUsedBytes() >= 0 && // sentinel: device exists in /proc/swaps
			zramInProcSwaps()
	}
	if s.Enabled {
		s.DiskSizeBytes = readSysFileInt64("/sys/block/zram0/disksize")
		s.OrigDataBytes, s.ComprDataBytes, s.MemUsedBytes = readZramMMStat()
		// Older kernels (≤ 5.x) had per-file counters; mm_stat became the
		// canonical interface in newer kernels (Debian 13 ships kernel 6.12
		// where the per-file counters return empty strings). Fall back to
		// the legacy paths only if mm_stat returned zeros — keeps backwards
		// compatibility without hiding the real data on stale kernels.
		if s.OrigDataBytes == 0 && s.ComprDataBytes == 0 {
			s.OrigDataBytes = readSysFileInt64("/sys/block/zram0/orig_data_size")
			s.ComprDataBytes = readSysFileInt64("/sys/block/zram0/compr_data_size")
			s.MemUsedBytes = readSysFileInt64("/sys/block/zram0/mem_used_total")
		}
		if s.ComprDataBytes > 0 {
			s.Ratio = float64(s.OrigDataBytes) / float64(s.ComprDataBytes)
		}
		s.SwapUsedBytes = readZramSwapUsedBytes()
	}
	return s
}

// readZramMMStat parses /sys/block/zram0/mm_stat. Format (kernel 4.10+):
//
//	orig_data_size compr_data_size mem_used_total mem_limit mem_used_max ...
//
// Fields are space-separated, in bytes. We only consume the first three.
// Returns zeros if the file is missing or unreadable.
func readZramMMStat() (orig, compr, memUsed int64) {
	data, err := os.ReadFile("/sys/block/zram0/mm_stat")
	if err != nil {
		return 0, 0, 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return 0, 0, 0
	}
	orig, _ = strconv.ParseInt(fields[0], 10, 64)
	compr, _ = strconv.ParseInt(fields[1], 10, 64)
	memUsed, _ = strconv.ParseInt(fields[2], 10, 64)
	return orig, compr, memUsed
}

// readTotalSwapUsedBytes returns the sum of "Used" across every swap entry
// in /proc/swaps — zram + on-disk swap partitions/files combined. This is
// the figure we want for the topbar pct readout: working-set pressure that
// has spilled out of physical RAM, regardless of where the pages landed.
func readTotalSwapUsedBytes() int64 {
	f, err := os.Open("/proc/swaps")
	if err != nil {
		return 0
	}
	defer f.Close()
	var totalKB int64
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Filename") {
			continue // header
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		// fields: Filename Type Size Used Priority
		usedKB, _ := strconv.ParseInt(fields[3], 10, 64)
		totalKB += usedKB
	}
	return totalKB * 1024
}

// ApplyMemCompConfig is the one entrypoint for enable / disable / reconfigure.
// The reconfigure path is the dangerous one: shrinking PERCENT while zram has
// data can ENOMEM during swapoff. We refuse it pre-flight.
func ApplyMemCompConfig(cfg MemCompConfig) error {
	if !MemCompPrereqsInstalled() {
		return fmt.Errorf("zram-tools is not installed; install it from Prerequisites first")
	}
	if cfg.Enabled {
		if cfg.PercentRAM < 5 || cfg.PercentRAM > 75 {
			return fmt.Errorf("percent_ram must be between 5 and 75 (got %d)", cfg.PercentRAM)
		}
		switch cfg.Algorithm {
		case "zstd", "lz4", "lzo":
		default:
			return fmt.Errorf("algorithm must be zstd, lz4, or lzo (got %q)", cfg.Algorithm)
		}
	}

	cur := GetMemCompStatus()

	// Reconfigure-safety: only when zram is currently enabled and the new
	// config either disables it OR shrinks the pool (smaller PercentRAM
	// or different algorithm — both force a swapoff first).
	if cur.Enabled {
		needSwapoff := !cfg.Enabled || cfg.PercentRAM < cur.PercentRAM || cfg.Algorithm != cur.Algorithm
		if needSwapoff && cur.OrigDataBytes > 0 {
			avail := readMemAvailableBytes()
			// Reserve 512 MiB as a safety margin for the kernel.
			const reserve = int64(512 * 1024 * 1024)
			if cur.OrigDataBytes > avail-reserve {
				return fmt.Errorf(
					"cannot re-create zram device safely: %s currently held in zram swap, only %s of physical RAM is free. Stop or shrink a VM and try again.",
					humanBytes(cur.OrigDataBytes), humanBytes(avail-reserve))
			}
		}
	}

	// Write the config file unconditionally (even on disable) so a future
	// `apt upgrade` of zram-tools doesn't silently re-arm the old setting.
	pct := cfg.PercentRAM
	algo := cfg.Algorithm
	if !cfg.Enabled {
		pct = 0
		if algo == "" {
			algo = "zstd"
		}
	}
	if err := writeMemCompConfigFile(pct, algo); err != nil {
		return err
	}

	if cfg.Enabled {
		// We deliberately do `stop` followed by `start` instead of `restart`.
		// zram-tools' service script can't shrink an in-use zram device — its
		// restart path triggers `Device or resource busy` errors on the size
		// reset and leaves /sys/block/zram0/disksize at the previous value.
		// Stop first ensures swapoff happens cleanly, freeing the device so
		// the next start can recreate it at the new PERCENT.
		if cur.Enabled {
			if out, err := exec.Command("sudo", "systemctl", "stop", memCompService).CombinedOutput(); err != nil {
				msg := strings.TrimSpace(string(out))
				if !strings.Contains(msg, "not loaded") {
					return fmt.Errorf("systemctl stop %s: %s", memCompService, msg)
				}
			}
		}
		if out, err := exec.Command("sudo", "systemctl", "start", memCompService).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl start %s: %s", memCompService, strings.TrimSpace(string(out)))
		}
		if out, err := exec.Command("sudo", "systemctl", "enable", memCompService).CombinedOutput(); err != nil {
			// Enable failure is non-fatal — the pool is up, but won't survive reboot.
			return fmt.Errorf("systemctl enable %s: %s (pool is active but won't survive reboot)",
				memCompService, strings.TrimSpace(string(out)))
		}
	} else {
		exec.Command("sudo", "systemctl", "disable", memCompService).Run() //nolint:errcheck
		if out, err := exec.Command("sudo", "systemctl", "stop", memCompService).CombinedOutput(); err != nil {
			// stop failure is mostly harmless: if zramswap was already inactive,
			// systemctl returns success; if it was active and stop failed, the
			// admin has a real problem and we surface it.
			msg := strings.TrimSpace(string(out))
			if !strings.Contains(msg, "not loaded") {
				return fmt.Errorf("systemctl stop %s: %s", memCompService, msg)
			}
		}
	}
	return nil
}

// InstallMemCompPrereqs runs `apt-get install -y zram-tools` and streams stdout
// to the supplied callback (one call per line). Returns nil on success.
func InstallMemCompPrereqs(stream func(string)) error {
	cmd := exec.Command("sudo", "apt-get", "install", "-y", "-q", memCompPackage)
	cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return err
	}
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		if stream != nil {
			stream(scanner.Text())
		}
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("apt-get install %s: %w", memCompPackage, err)
	}
	return nil
}

// ── internals ──────────────────────────────────────────────────────────────

// readMemCompConfigFile parses /etc/default/zramswap. Format is shell-style
// KEY=VALUE, one per line; we only care about PERCENT and ALGO. Unknown keys
// are ignored (PRIORITY is set by us but not surfaced in the UI).
// Returns ok=false if the file is missing.
func readMemCompConfigFile() (percent int, algo string, ok bool) {
	f, err := os.Open(memCompConfigPath)
	if err != nil {
		return 0, "", false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := strings.Trim(strings.TrimSpace(line[eq+1:]), "\"'")
		switch key {
		case "PERCENT":
			if n, e := strconv.Atoi(val); e == nil {
				percent = n
			}
		case "ALGO":
			algo = val
		}
	}
	return percent, algo, true
}

// writeMemCompConfigFile rewrites /etc/default/zramswap atomically via
// sudo tee. The `tee` path is in the canonical sudoers allowlist for this
// exact file.
func writeMemCompConfigFile(percent int, algo string) error {
	body := fmt.Sprintf(
		"# Managed by ZNAS — Settings → Virtualization → Memory Compression.\n"+
			"# PERCENT=0 means the zram pool is disabled.\n"+
			"ALGO=%s\nPERCENT=%d\nPRIORITY=100\n", algo, percent)
	cmd := exec.Command("sudo", "tee", memCompConfigPath)
	cmd.Stdin = strings.NewReader(body)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tee %s: %s", memCompConfigPath, strings.TrimSpace(string(out)))
	}
	return nil
}

// isSystemdActive probes `systemctl is-active <unit>` (read-only, no sudo).
func isSystemdActive(unit string) bool {
	out, _ := exec.Command("systemctl", "is-active", unit).Output()
	return strings.TrimSpace(string(out)) == "active"
}

// readSysFileInt64 reads a single integer from a sysfs file; returns 0 on
// any error. Used for the /sys/block/zram0/* counters.
func readSysFileInt64(path string) int64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}

// zramInProcSwaps reports whether /dev/zram* is listed in /proc/swaps.
// True ⇒ the kernel has the device armed as a swap device, regardless of
// what systemctl thinks of zramswap.service.
func zramInProcSwaps() bool {
	f, err := os.Open("/proc/swaps")
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) > 0 && strings.HasPrefix(fields[0], "/dev/zram") {
			return true
		}
	}
	return false
}

// readZramSwapUsedBytes scans /proc/swaps for the /dev/zram0 line and returns
// the Used column (KB) as bytes. Returns 0 if zram0 isn't currently a swap.
func readZramSwapUsedBytes() int64 {
	f, err := os.Open("/proc/swaps")
	if err != nil {
		return 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		if !strings.HasPrefix(fields[0], "/dev/zram") {
			continue
		}
		usedKB, _ := strconv.ParseInt(fields[3], 10, 64)
		return usedKB * 1024
	}
	return 0
}

// readMemAvailableBytes returns MemAvailable from /proc/meminfo, in bytes.
// Used by the reconfigure-safety check: it's the kernel's own estimate of
// memory we can allocate without going to swap.
func readMemAvailableBytes() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "MemAvailable:" {
			kb, _ := strconv.ParseInt(fields[1], 10, 64)
			return kb * 1024
		}
	}
	return 0
}

// humanBytes formats a byte count for the user-facing reconfigure error
// message. Stays terse: 4 GB, 850 MB, etc.
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/float64(1<<20))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
