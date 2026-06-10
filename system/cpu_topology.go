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

	allPerformance := func() *CPUTopology {
		for _, c := range cpus {
			topo.PCores = append(topo.PCores, c.id)
		}
		return topo
	}

	if len(freqSet) < 2 {
		return allPerformance()
	}

	var maxFreq, minFreq uint64
	for f := range freqSet {
		if f > maxFreq {
			maxFreq = f
		}
		if minFreq == 0 || f < minFreq {
			minFreq = f
		}
	}

	// A genuine P/E hybrid clocks its efficiency cores well below the
	// performance cores. Minor per-core max-frequency variation — Intel Turbo
	// Boost Max 3.0 / ITMT "favored cores", AMD preferred cores, or silicon
	// binning — is only a few percent and must NOT be read as an efficiency
	// tier, or a homogeneous host wrongly offers an "Efficient Cores" option.
	// Require at least a 15% spread between the slowest and fastest core before
	// classifying the CPU as hybrid.
	if minFreq*100 >= maxFreq*85 {
		return allPerformance()
	}

	topo.Hybrid = true
	// Split at the midpoint so the fast tier (P-cores, including any favored
	// core boosted slightly above its peers) lands in PCores and the slow tier
	// in ECores — rather than treating only the single highest frequency as P.
	mid := (maxFreq + minFreq) / 2
	for _, c := range cpus {
		if c.freq > mid {
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

// CPUIdsToLXD converts a slice of CPU IDs to a LXD limits.cpu pin string.
// e.g. [0,1,2,3,8,9,10,11] → "0-3,8-11"
//
// LXD distinguishes a CPU pin from a vCPU count by the presence of "," or "-".
// A single bare integer like "5" is interpreted as "5 vCPUs", not "pinned to
// CPU 5". When the input is a single CPU id we emit "N-N" so LXD pins to that
// one core (giving 1 vCPU) rather than allocating N vCPUs.
func CPUIdsToLXD(ids []int) string {
	if len(ids) == 0 {
		return ""
	}
	sorted := make([]int, len(ids))
	copy(sorted, ids)
	sort.Ints(sorted)

	var parts []string
	start, end := sorted[0], sorted[0]
	flush := func() {
		if start == end {
			// Use "N-N" to force pin semantics for a single CPU.
			parts = append(parts, strconv.Itoa(start)+"-"+strconv.Itoa(end))
		} else {
			parts = append(parts, strconv.Itoa(start)+"-"+strconv.Itoa(end))
		}
	}
	for _, id := range sorted[1:] {
		if id == end+1 {
			end = id
		} else {
			flush()
			start, end = id, id
		}
	}
	flush()
	out := strings.Join(parts, ",")
	// Single-CPU case: ensure the result has at least one "-" or "," so LXD
	// reads it as a pin instead of a count. flush() already does this for a
	// single element, but guard explicitly in case a future caller bypasses it.
	if !strings.ContainsAny(out, "-,") {
		out = out + "-" + out
	}
	return out
}

// normalizeCPUPin normalizes a user-supplied LXD limits.cpu pin string.
// LXD interprets a bare integer as a vCPU count rather than a CPU pin, so
// users typing a single CPU index (e.g. "5") would get "5 vCPUs" instead of
// "pinned to CPU 5". This helper rewrites a bare positive integer to "N-N";
// values that already contain "," or "-" pass through unchanged.
func normalizeCPUPin(s string) string {
	t := strings.TrimSpace(s)
	if t == "" {
		return s
	}
	if strings.ContainsAny(t, ",-") {
		return t
	}
	if _, err := strconv.Atoi(t); err == nil {
		return t + "-" + t
	}
	return t
}
