package system

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// CPUTopology describes the physical CPU topology of the host.
type CPUTopology struct {
	TotalCPUs int   `json:"total_cpus"`
	PCores    []int `json:"p_cores"` // high-performance cores (or all if not hybrid)
	ECores    []int `json:"e_cores"` // efficiency cores (empty if not hybrid)
	Hybrid    bool  `json:"hybrid"`  // true when P+E cores detected
}

// GetCPUTopology detects P-core and E-core CPU IDs.
// Primary source: /sys/devices/cpu_core/cpus and /sys/devices/cpu_atom/cpus (Intel hybrid).
// Fallback: compare cpuinfo_max_freq across logical CPUs.
func GetCPUTopology() (*CPUTopology, error) {
	if topo := getTopologyFromSysDevices(); topo != nil {
		return topo, nil
	}
	return getTopologyFromFreq(), nil
}

// getTopologyFromSysDevices reads the kernel's authoritative cpu_core / cpu_atom sets.
func getTopologyFromSysDevices() *CPUTopology {
	coreData, coreErr := os.ReadFile("/sys/devices/cpu_core/cpus")
	atomData, atomErr := os.ReadFile("/sys/devices/cpu_atom/cpus")
	if coreErr != nil || atomErr != nil {
		return nil
	}
	pcores := parseCPUSet(strings.TrimSpace(string(coreData)))
	ecores := parseCPUSet(strings.TrimSpace(string(atomData)))
	if len(pcores) == 0 {
		return nil
	}
	sort.Ints(pcores)
	sort.Ints(ecores)
	return &CPUTopology{
		TotalCPUs: len(pcores) + len(ecores),
		PCores:    pcores,
		ECores:    ecores,
		Hybrid:    len(ecores) > 0,
	}
}

// parseCPUSet parses a kernel cpuset string like "0-11,16-19" into a sorted []int.
func parseCPUSet(s string) []int {
	var ids []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if dash := strings.Index(part, "-"); dash >= 0 {
			lo, err1 := strconv.Atoi(part[:dash])
			hi, err2 := strconv.Atoi(part[dash+1:])
			if err1 != nil || err2 != nil {
				continue
			}
			for i := lo; i <= hi; i++ {
				ids = append(ids, i)
			}
		} else {
			id, err := strconv.Atoi(part)
			if err == nil {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// getTopologyFromFreq detects core types by comparing cpuinfo_max_freq values.
func getTopologyFromFreq() *CPUTopology {
	entries, err := filepath.Glob("/sys/devices/system/cpu/cpu[0-9]*/cpufreq/cpuinfo_max_freq")
	if err != nil || len(entries) == 0 {
		return fallbackTopology()
	}

	type cpuFreq struct {
		id   int
		freq uint64
	}
	var cpus []cpuFreq
	for _, path := range entries {
		parts := strings.Split(filepath.Dir(filepath.Dir(path)), "/")
		if len(parts) == 0 {
			continue
		}
		name := parts[len(parts)-1]
		id, err := strconv.Atoi(strings.TrimPrefix(name, "cpu"))
		if err != nil {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		freq, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
		if err != nil {
			continue
		}
		cpus = append(cpus, cpuFreq{id: id, freq: freq})
	}

	if len(cpus) == 0 {
		return fallbackTopology()
	}

	sort.Slice(cpus, func(i, j int) bool { return cpus[i].id < cpus[j].id })

	freqSet := map[uint64]struct{}{}
	for _, c := range cpus {
		freqSet[c.freq] = struct{}{}
	}

	topo := &CPUTopology{TotalCPUs: len(cpus)}

	if len(freqSet) < 2 {
		for _, c := range cpus {
			topo.PCores = append(topo.PCores, c.id)
		}
		return topo
	}

	topo.Hybrid = true
	var maxFreq uint64
	for f := range freqSet {
		if f > maxFreq {
			maxFreq = f
		}
	}
	for _, c := range cpus {
		if c.freq == maxFreq {
			topo.PCores = append(topo.PCores, c.id)
		} else {
			topo.ECores = append(topo.ECores, c.id)
		}
	}
	return topo
}

func fallbackTopology() *CPUTopology {
	data, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return &CPUTopology{TotalCPUs: 1}
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "processor") {
			count++
		}
	}
	if count == 0 {
		count = 1
	}
	ids := make([]int, count)
	for i := range ids {
		ids[i] = i
	}
	return &CPUTopology{TotalCPUs: count, PCores: ids}
}

// CPUIdsToLXD converts a slice of CPU IDs to a LXD limits.cpu range string.
// e.g. [0,1,2,3,8,9,10,11] → "0-3,8-11"
func CPUIdsToLXD(ids []int) string {
	if len(ids) == 0 {
		return ""
	}
	sorted := make([]int, len(ids))
	copy(sorted, ids)
	sort.Ints(sorted)

	var parts []string
	start, end := sorted[0], sorted[0]
	for _, id := range sorted[1:] {
		if id == end+1 {
			end = id
		} else {
			if start == end {
				parts = append(parts, strconv.Itoa(start))
			} else {
				parts = append(parts, strconv.Itoa(start)+"-"+strconv.Itoa(end))
			}
			start, end = id, id
		}
	}
	if start == end {
		parts = append(parts, strconv.Itoa(start))
	} else {
		parts = append(parts, strconv.Itoa(start)+"-"+strconv.Itoa(end))
	}
	return strings.Join(parts, ",")
}
