package system

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProcMemInfo is a single process memory entry. Used for both the TopProcs
// list (sorted by RSS) and the TopSwapProcs list (sorted by VmSwap, v6.5.3+).
// In the swap variant MemMB / MemPct still report the resident size so the
// SWAP tooltip can show RSS alongside the swap bytes for context, while
// SwapMB / SwapPct are populated for both lists.
type ProcMemInfo struct {
	PID      int     `json:"pid"`
	Name     string  `json:"name"`
	Cmd      string  `json:"cmd"`
	MemMB    float64 `json:"mem_mb"`
	MemPct   float64 `json:"mem_pct"`
	SwapMB   float64 `json:"swap_mb,omitempty"`
	SwapPct  float64 `json:"swap_pct,omitempty"`
	Category string  `json:"category"`
}

// MemProcsSnapshot is the full memory snapshot returned by the API.
type MemProcsSnapshot struct {
	SmbPct       float64       `json:"smb_pct"`
	NfsPct       float64       `json:"nfs_pct"`
	ZfsPct       float64       `json:"zfs_pct"` // ZFS processes + ARC cache
	MinioPct     float64       `json:"minio_pct"`
	ISCSIPct     float64       `json:"iscsi_pct"`
	VMPct        float64       `json:"vm_pct"`
	ContainerPct float64       `json:"container_pct"`
	OtherPct     float64       `json:"other_pct"`
	ArcMB        float64       `json:"arc_mb"`
	TotalMB      float64       `json:"total_mb"`
	UsedMB       float64       `json:"used_mb"`
	TopProcs     []ProcMemInfo `json:"top_procs"`
	// Top 10 processes sorted by VmSwap descending (v6.5.3+). Drives the
	// SWAP topbar tooltip so users can see which processes have the most
	// pages currently in any swap device.
	TopSwapProcs []ProcMemInfo `json:"top_swap_procs"`
	At           time.Time     `json:"at"`
	// Memory compression fields (v6.5.3+). All zero when zram is disabled
	// or zram-tools isn't installed — UI uses ZramActive to decide whether
	// to draw the compressed-pool extension on the topbar gauge.
	ZramActive  bool    `json:"zram_active"`
	ZramPoolMB  float64 `json:"zram_pool_mb"`  // configured cap (PERCENT × MemTotal)
	ZramOrigMB  float64 `json:"zram_orig_mb"`  // uncompressed bytes held in zram
	ZramComprMB float64 `json:"zram_compr_mb"` // compressed bytes physically in RAM
	ZramRatio   float64 `json:"zram_ratio"`    // orig / max(compr, 1)
	// TotalSwapUsedMB is the sum of Used across every swap device in
	// /proc/swaps (zram + any on-disk swap). Used by the topbar gauge's
	// percentage readout to compute (used+swap)/total — when the
	// working set has spilled out of physical RAM the pct rises past
	// 100 % regardless of which swap backend caught the spill.
	TotalSwapUsedMB float64 `json:"total_swap_used_mb"`
	// Swap topbar gauge fields (v6.5.3+). Per-category percentages are
	// the share of TOTAL SWAP CAPACITY each process category currently
	// holds, summed from /proc/<pid>/status VmSwap. SwapTotalMB / Used
	// drive the bar's overall fill level + the green/yellow/red readout.
	// All zero when no swap device is configured (SwapTotalMB == 0); the
	// UI hides the swap bar in that case.
	SwapTotalMB     float64 `json:"swap_total_mb"`
	SwapUsedMB      float64 `json:"swap_used_mb"`     // mirrors TotalSwapUsedMB; kept here for the bar's own % math
	// Disk-only / zram-only split (v6.5.3+). The SWAP vertical bar is now
	// dedicated to real disk swap so it conveys "we're paging out to NVMe"
	// rather than mixing in compressed-RAM activity. zram details live on
	// the MEM bar (compressed segment + tooltip row); the SWAP tooltip
	// still echoes them on its second row.
	DiskSwapTotalMB  float64 `json:"disk_swap_total_mb"`
	DiskSwapUsedMB   float64 `json:"disk_swap_used_mb"`
	ZramSwapTotalMB  float64 `json:"zram_swap_total_mb"`
	ZramSwapUsedMB   float64 `json:"zram_swap_used_mb"`
	SmbSwapPct      float64 `json:"smb_swap_pct"`
	NfsSwapPct      float64 `json:"nfs_swap_pct"`
	ZfsSwapPct      float64 `json:"zfs_swap_pct"`
	MinioSwapPct    float64 `json:"minio_swap_pct"`
	ISCSISwapPct    float64 `json:"iscsi_swap_pct"`
	VMSwapPct       float64 `json:"vm_swap_pct"`
	ContainerSwapPct float64 `json:"container_swap_pct"`
	OtherSwapPct    float64 `json:"other_swap_pct"`
}

var (
	memProcsMu     sync.RWMutex
	memProcsLatest *MemProcsSnapshot
)

// GetMemProcsSnapshot returns the latest memory snapshot (nil until first sample).
func GetMemProcsSnapshot() *MemProcsSnapshot {
	memProcsMu.RLock()
	defer memProcsMu.RUnlock()
	return memProcsLatest
}

// StartMemProcsPoller samples per-process memory usage every 5 s.
func StartMemProcsPoller() {
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for range tick.C {
			snap := sampleMemProcs()
			if snap != nil {
				memProcsMu.Lock()
				memProcsLatest = snap
				memProcsMu.Unlock()
			}
		}
	}()
}

func sampleMemProcs() *MemProcsSnapshot {
	totalKB, availKB, err := readMemInfo()
	if err != nil || totalKB == 0 {
		return nil
	}
	totalMB := float64(totalKB) / 1024.0
	usedKB := totalKB - availKB
	usedMB := float64(usedKB) / 1024.0

	arcKB := readARCSize()
	arcMB := float64(arcKB) / 1024.0

	procDir, err := os.Open("/proc")
	if err != nil {
		return nil
	}
	entries, err := procDir.ReadDir(-1)
	procDir.Close()
	if err != nil {
		return nil
	}

	type procEntry struct {
		pid      int
		name     string
		rssKB    uint64
		swapKB   uint64
		shmemKB  uint64
		category string
	}

	var procs []procEntry
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		name, rssKB, swapKB, shmemKB, err := readProcMemStatus(pid)
		if err != nil {
			continue
		}
		// Keep processes with either residency or anonymous-swap presence —
		// purely-swapped ones (rssKB = 0 but swapKB > 0) belong on the swap
		// gauge even when the MEM gauge ignores them.
		if rssKB == 0 && swapKB == 0 {
			continue
		}
		procs = append(procs, procEntry{
			pid:      pid,
			name:     name,
			rssKB:    rssKB,
			swapKB:   swapKB,
			shmemKB:  shmemKB,
			category: categorizeProc(pid, name),
		})
	}

	// Upgrade swap accounting for shmem-heavy processes (notably QEMU, whose
	// guest RAM is shmem-backed). VmSwap from /proc/<pid>/status counts only
	// anonymous private swap, so a 23 GB VM with 5 GB paged out shows up as
	// VmSwap=45 MB while smaps_rollup correctly reports 5 GB. Threshold of
	// 256 MB shmem keeps the cost negligible — typically just QEMU/X server.
	for i := range procs {
		if procs[i].shmemKB < 256*1024 {
			continue
		}
		if total, ok := readSmapsRollupSwapKB(procs[i].pid); ok && total > procs[i].swapKB {
			procs[i].swapKB = total
		}
	}

	// Sort by RSS descending
	sort.Slice(procs, func(i, j int) bool {
		return procs[i].rssKB > procs[j].rssKB
	})

	// Aggregate per-category in two parallel maps: one for resident memory
	// (drives the MEM gauge), one for swap (drives the new swap gauge in
	// v6.5.3+). Same category set for both so the bar legends are uniform.
	catKB := map[string]uint64{
		CpuCatSMB:       0,
		CpuCatNFS:       0,
		CpuCatZFS:       0,
		CpuCatMinIO:     0,
		CpuCatISCSI:     0,
		CpuCatVM:        0,
		CpuCatContainer: 0,
		CpuCatOther:     0,
	}
	catSwapKB := map[string]uint64{
		CpuCatSMB:       0,
		CpuCatNFS:       0,
		CpuCatZFS:       0,
		CpuCatMinIO:     0,
		CpuCatISCSI:     0,
		CpuCatVM:        0,
		CpuCatContainer: 0,
		CpuCatOther:     0,
	}
	for _, p := range procs {
		catKB[p.category] += p.rssKB
		catSwapKB[p.category] += p.swapKB
	}
	// Add ARC to the ZFS bucket (resident only — ARC isn't swappable).
	catKB[CpuCatZFS] += arcKB

	// Reconcile per-category RSS+ARC totals with /proc/meminfo's "used"
	// (= MemTotal − MemAvailable). The categorised buckets only see what
	// /proc/<pid>/status exposes as RSS plus ARC; slab caches, kernel
	// buffers, hugepages, KVM EPT pages, and a handful of anonymous-page
	// edge cases aren't visible there but very much count toward usedKB.
	// Without this fold-in, the readout (used/total) and the bar segments
	// (sum of categorised RSS) diverge — typically by 10-20 % — making the
	// MEM gauge look ~half full when the system actually reports 60-70 %
	// used. Folding the remainder into "Other" keeps each named bucket
	// accurate (RSS+ARC where applicable) while letting the bar's filled
	// height match the headline percentage.
	var trackedKB uint64
	for _, kb := range catKB {
		trackedKB += kb
	}
	if usedKB > trackedKB {
		catKB[CpuCatOther] += usedKB - trackedKB
	}

	// Split /proc/swaps into disk vs zram. The SWAP bar is dedicated to
	// real disk swap (the "we're paging to NVMe" signal); zram swap is
	// surfaced separately on the MEM bar's compressed segment + tooltip
	// row, and echoed on the SWAP tooltip's second row.
	diskSwapTotalKB, diskSwapUsedKB, zramSwapTotalKB, zramSwapUsedKB := readSwapTotalUsedKBSplit()
	swapTotalKB := diskSwapTotalKB + zramSwapTotalKB
	swapUsedKB := diskSwapUsedKB + zramSwapUsedKB

	// Per-category swap segments are derived from /proc/<pid>/status VmSwap,
	// which mixes zram and disk pages — the kernel doesn't expose the split.
	// To keep the bar honest we project the categorised totals onto disk-only
	// usage proportionally: each category keeps its share of the tracked
	// VmSwap, but the absolute KB are scaled to fit within diskSwapUsedKB.
	if zramSwapTotalKB > 0 && diskSwapTotalKB > 0 {
		var trackedSwapKB uint64
		for _, kb := range catSwapKB {
			trackedSwapKB += kb
		}
		if trackedSwapKB > 0 {
			scale := float64(diskSwapUsedKB) / float64(trackedSwapKB)
			for cat, kb := range catSwapKB {
				catSwapKB[cat] = uint64(float64(kb) * scale)
			}
		} else {
			for cat := range catSwapKB {
				catSwapKB[cat] = 0
			}
		}
	} else if diskSwapTotalKB == 0 {
		// zram-only host (or no swap at all) — clear the per-category swap
		// shares so the (now-hidden) bar isn't seeded with stale numbers.
		for cat := range catSwapKB {
			catSwapKB[cat] = 0
		}
	}

	// Reconcile per-category swap totals with the disk swap "Used". The
	// summed VmSwap across living processes is always ≤ what the kernel
	// reports because orphaned anonymous pages (from processes that exited
	// while their pages were still in swap) and a few uncommon page types
	// aren't visible through any single /proc/<pid>/status. Fold the
	// untracked remainder into "Other" so the bar's filled height matches
	// the headline readout.
	if diskSwapUsedKB > 0 {
		var trackedKB uint64
		for _, kb := range catSwapKB {
			trackedKB += kb
		}
		if diskSwapUsedKB > trackedKB {
			catSwapKB[CpuCatOther] += diskSwapUsedKB - trackedKB
		}
	}

	pct := func(kb uint64) float64 {
		return float64(kb) / float64(totalKB) * 100.0
	}

	// Top 10 processes for the MEM tooltip — sorted by RSS, ARC ignored
	// (it's not a process).
	top := procs
	if len(top) > 10 {
		top = top[:10]
	}
	topProcs := make([]ProcMemInfo, len(top))
	for i, p := range top {
		topProcs[i] = ProcMemInfo{
			PID:      p.pid,
			Name:     p.name,
			Cmd:      readProcCmdline(p.pid),
			MemMB:    float64(p.rssKB) / 1024.0,
			MemPct:   pct(p.rssKB),
			SwapMB:   float64(p.swapKB) / 1024.0,
			Category: p.category,
		}
	}

	// Top 10 processes by VmSwap (v6.5.3+) for the SWAP tooltip. Sorted
	// independently — many processes with low RSS hold the bulk of swapped
	// pages (e.g. idle VMs whose memory was paged out). Returns an empty
	// slice when no swap is in use.
	swapSorted := make([]procEntry, len(procs))
	copy(swapSorted, procs)
	sort.Slice(swapSorted, func(i, j int) bool {
		return swapSorted[i].swapKB > swapSorted[j].swapKB
	})
	swapTop := swapSorted
	if len(swapTop) > 10 {
		swapTop = swapTop[:10]
	}
	topSwapProcs := make([]ProcMemInfo, 0, len(swapTop))
	for _, p := range swapTop {
		if p.swapKB == 0 {
			break // descending order — first zero means everything below is too
		}
		var sp float64
		if swapTotalKB > 0 {
			sp = float64(p.swapKB) / float64(swapTotalKB) * 100.0
		}
		topSwapProcs = append(topSwapProcs, ProcMemInfo{
			PID:      p.pid,
			Name:     p.name,
			Cmd:      readProcCmdline(p.pid),
			MemMB:    float64(p.rssKB) / 1024.0,
			MemPct:   pct(p.rssKB),
			SwapMB:   float64(p.swapKB) / 1024.0,
			SwapPct:  sp,
			Category: p.category,
		})
	}

	// Read zram counters once per sample. Cheap (1 mm_stat read + 1
	// /proc/swaps scan) and gives the topbar gauge + dashboard chart
	// enough state to draw the compressed-pool extension without a
	// separate API roundtrip.
	zc := GetMemCompStatus()
	const mb = float64(1 << 20)
	swapTotalMB := float64(swapTotalKB) / 1024.0
	swapUsedMB := float64(swapUsedKB) / 1024.0
	diskSwapTotalMB := float64(diskSwapTotalKB) / 1024.0
	diskSwapUsedMB  := float64(diskSwapUsedKB) / 1024.0
	zramSwapTotalMB := float64(zramSwapTotalKB) / 1024.0
	zramSwapUsedMB  := float64(zramSwapUsedKB) / 1024.0
	// Per-category swap share = % of disk swap capacity, since the bar now
	// shows only real disk swap. Returns 0 on hosts without disk swap.
	swapPct := func(kb uint64) float64 {
		if diskSwapTotalKB == 0 {
			return 0
		}
		return float64(kb) / float64(diskSwapTotalKB) * 100.0
	}

	return &MemProcsSnapshot{
		SmbPct:       pct(catKB[CpuCatSMB]),
		NfsPct:       pct(catKB[CpuCatNFS]),
		ZfsPct:       pct(catKB[CpuCatZFS]),
		MinioPct:     pct(catKB[CpuCatMinIO]),
		ISCSIPct:     pct(catKB[CpuCatISCSI]),
		VMPct:        pct(catKB[CpuCatVM]),
		ContainerPct: pct(catKB[CpuCatContainer]),
		OtherPct:     pct(catKB[CpuCatOther]),
		ArcMB:        arcMB,
		TotalMB:      totalMB,
		UsedMB:       usedMB,
		TopProcs:     topProcs,
		TopSwapProcs: topSwapProcs,
		At:           time.Now(),
		ZramActive:      zc.Enabled,
		ZramPoolMB:      float64(zc.DiskSizeBytes) / mb,
		ZramOrigMB:      float64(zc.OrigDataBytes) / mb,
		ZramComprMB:     float64(zc.ComprDataBytes) / mb,
		ZramRatio:       zc.Ratio,
		TotalSwapUsedMB: swapUsedMB,
		// Swap gauge fields (v6.5.3+).
		SwapTotalMB:      swapTotalMB,
		SwapUsedMB:       swapUsedMB,
		DiskSwapTotalMB:  diskSwapTotalMB,
		DiskSwapUsedMB:   diskSwapUsedMB,
		ZramSwapTotalMB:  zramSwapTotalMB,
		ZramSwapUsedMB:   zramSwapUsedMB,
		SmbSwapPct:       swapPct(catSwapKB[CpuCatSMB]),
		NfsSwapPct:       swapPct(catSwapKB[CpuCatNFS]),
		ZfsSwapPct:       swapPct(catSwapKB[CpuCatZFS]),
		MinioSwapPct:     swapPct(catSwapKB[CpuCatMinIO]),
		ISCSISwapPct:     swapPct(catSwapKB[CpuCatISCSI]),
		VMSwapPct:        swapPct(catSwapKB[CpuCatVM]),
		ContainerSwapPct: swapPct(catSwapKB[CpuCatContainer]),
		OtherSwapPct:     swapPct(catSwapKB[CpuCatOther]),
	}
}

// readSwapTotalUsedKB returns (total, used) summed across every active swap
// device in /proc/swaps, in KB. Returns (0, 0) when no swap is configured.
// Used by the swap gauge — it needs both the cap (for the bar's 100 %) and
// the in-use bytes (for the green/yellow/red readout).
func readSwapTotalUsedKB() (totalKB, usedKB uint64) {
	totalKB, usedKB, _, _ = readSwapTotalUsedKBSplit()
	return totalKB, usedKB
}

// readSwapTotalUsedKBSplit returns (total, used) for both real disk swap and
// zram swap separately. /dev/zram* entries in /proc/swaps go into the zram
// bucket; everything else is disk. The split lets the SWAP bar render only
// real-storage paging while the MEM bar/tooltip handles compressed memory.
func readSwapTotalUsedKBSplit() (diskTotalKB, diskUsedKB, zramTotalKB, zramUsedKB uint64) {
	f, err := os.Open("/proc/swaps")
	if err != nil {
		return 0, 0, 0, 0
	}
	defer f.Close()
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
		t, _ := strconv.ParseUint(fields[2], 10, 64)
		u, _ := strconv.ParseUint(fields[3], 10, 64)
		if strings.HasPrefix(fields[0], "/dev/zram") {
			zramTotalKB += t
			zramUsedKB += u
		} else {
			diskTotalKB += t
			diskUsedKB += u
		}
	}
	return diskTotalKB, diskUsedKB, zramTotalKB, zramUsedKB
}

// readMemInfo reads MemTotal and MemAvailable from /proc/meminfo (values in kB).
func readMemInfo() (totalKB, availKB uint64, err error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			totalKB = v
		case "MemAvailable:":
			availKB = v
		}
		if totalKB > 0 && availKB > 0 {
			break
		}
	}
	if totalKB == 0 {
		return 0, 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
	}
	return totalKB, availKB, nil
}

// readProcMemStatus reads process Name, VmRSS, and VmSwap from /proc/[pid]/status.
// VmSwap (v6.5.3+) is the per-process bytes currently in any swap device — used
// to drive the per-category breakdown of the topbar swap gauge. Kernel threads
// (no VmSwap line) report swapKB = 0.
func readProcMemStatus(pid int) (name string, rssKB, swapKB, shmemKB uint64, err error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return "", 0, 0, 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "Name:") {
			name = strings.TrimSpace(strings.TrimPrefix(line, "Name:"))
		} else if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				rssKB, _ = strconv.ParseUint(fields[1], 10, 64)
			}
		} else if strings.HasPrefix(line, "VmSwap:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				swapKB, _ = strconv.ParseUint(fields[1], 10, 64)
			}
		} else if strings.HasPrefix(line, "RssShmem:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				shmemKB, _ = strconv.ParseUint(fields[1], 10, 64)
			}
		}
	}
	if name == "" {
		return "", 0, 0, 0, fmt.Errorf("no Name in /proc/%d/status", pid)
	}
	return name, rssKB, swapKB, shmemKB, nil
}

// readSmapsRollupSwapKB returns the total Swap (in KB) across every VMA of a
// process, summed by the kernel in /proc/<pid>/smaps_rollup. Unlike VmSwap in
// /proc/<pid>/status, this includes shared-memory mappings — important for
// QEMU/KVM workers whose guest RAM is shmem-backed (multi-GB of shmem swap is
// invisible to VmSwap). Tries a direct read first; falls back to `sudo -n cat`
// for processes owned by another user (zfsnas can't open smaps_rollup of
// nobody-owned QEMU processes without elevation). Returns (0, false) when
// neither path works — caller keeps VmSwap as the best available figure.
func readSmapsRollupSwapKB(pid int) (uint64, bool) {
	path := fmt.Sprintf("/proc/%d/smaps_rollup", pid)
	data, err := os.ReadFile(path)
	if err != nil {
		out, err2 := exec.Command("sudo", "-n", "/usr/bin/cat", path).Output()
		if err2 != nil {
			return 0, false
		}
		data = out
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "Swap:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, perr := strconv.ParseUint(fields[1], 10, 64)
				if perr == nil {
					return kb, true
				}
			}
			break
		}
	}
	return 0, false
}
