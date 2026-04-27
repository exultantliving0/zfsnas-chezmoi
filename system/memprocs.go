package system

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProcMemInfo is a single process memory entry.
type ProcMemInfo struct {
	PID      int     `json:"pid"`
	Name     string  `json:"name"`
	Cmd      string  `json:"cmd"`
	MemMB    float64 `json:"mem_mb"`
	MemPct   float64 `json:"mem_pct"`
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
	At           time.Time     `json:"at"`
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
		name, rssKB, err := readProcMemStatus(pid)
		if err != nil || rssKB == 0 {
			continue
		}
		procs = append(procs, procEntry{
			pid:      pid,
			name:     name,
			rssKB:    rssKB,
			category: categorizeProc(pid, name),
		})
	}

	// Sort by RSS descending
	sort.Slice(procs, func(i, j int) bool {
		return procs[i].rssKB > procs[j].rssKB
	})

	// Aggregate by category (as % of total RAM)
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
	for _, p := range procs {
		catKB[p.category] += p.rssKB
	}
	// Add ARC to the ZFS bucket
	catKB[CpuCatZFS] += arcKB

	pct := func(kb uint64) float64 {
		return float64(kb) / float64(totalKB) * 100.0
	}

	// Top 10 processes for tooltip
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
			Category: p.category,
		}
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
		At:           time.Now(),
	}
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

// readProcMemStatus reads process name and VmRSS from /proc/[pid]/status.
func readProcMemStatus(pid int) (name string, rssKB uint64, err error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return "", 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "Name:") {
			name = strings.TrimSpace(strings.TrimPrefix(line, "Name:"))
		} else if strings.HasPrefix(line, "VmRSS:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				rssKB, _ = strconv.ParseUint(fields[1], 10, 64)
			}
		}
		if name != "" && rssKB > 0 {
			break
		}
	}
	if name == "" {
		return "", 0, fmt.Errorf("no Name in /proc/%d/status", pid)
	}
	return name, rssKB, nil
}
