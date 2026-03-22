package system

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// ARCStats holds the current ARC (Adaptive Replacement Cache) statistics
// and tunable parameters read from the kernel.
type ARCStats struct {
	// Current sizes (bytes)
	ARCSize       int64 `json:"arc_size"`
	ARCMin        int64 `json:"arc_min"`
	ARCMax        int64 `json:"arc_max"`
	ARCTarget     int64 `json:"arc_target"`
	ARCMetaUsed   int64 `json:"arc_meta_used"`
	ARCMetaLimit  int64 `json:"arc_meta_limit"`
	ARCMetaMax    int64 `json:"arc_meta_max"`
	MFUSize       int64 `json:"mfu_size"`
	MRUSize       int64 `json:"mru_size"`

	// Hit/miss counters
	Hits             int64   `json:"hits"`
	Misses           int64   `json:"misses"`
	HitRatio         float64 `json:"hit_ratio"`
	DemandDataHits   int64   `json:"demand_data_hits"`
	DemandMetaHits   int64   `json:"demand_meta_hits"`
	PrefetchDataHits int64   `json:"prefetch_data_hits"`

	// Eviction counters
	EvictedMFU int64 `json:"evicted_mfu"`
	EvictedMRU int64 `json:"evicted_mru"`

	// L2ARC counters
	L2Hits   int64 `json:"l2_hits"`
	L2Misses int64 `json:"l2_misses"`
	L2Size   int64 `json:"l2_size"`

	// Tunable kernel parameters (from /sys/module/zfs/parameters/)
	ParamARCMax int64 `json:"param_arc_max"`
	ParamARCMin int64 `json:"param_arc_min"`
}

const arcStatsPath = "/proc/spl/kstat/zfs/arcstats"
const arcParamDir  = "/sys/module/zfs/parameters/"
const modprobePath = "/etc/modprobe.d/zfs.conf"

// GetARCStats reads /proc/spl/kstat/zfs/arcstats and kernel sysfs parameters,
// returning a populated ARCStats struct.
func GetARCStats() (*ARCStats, error) {
	kstat, err := readKstat(arcStatsPath)
	if err != nil {
		return nil, fmt.Errorf("read arcstats: %w", err)
	}

	s := &ARCStats{}

	s.ARCSize      = kstatInt(kstat, "size")
	s.ARCMin       = kstatInt(kstat, "c_min")
	s.ARCMax       = kstatInt(kstat, "c_max")
	s.ARCTarget    = kstatInt(kstat, "c")
	s.ARCMetaUsed  = kstatInt(kstat, "arc_meta_used")
	s.ARCMetaLimit = kstatInt(kstat, "arc_meta_limit")
	s.ARCMetaMax   = kstatInt(kstat, "arc_meta_max")
	s.MFUSize      = kstatInt(kstat, "mfu_size")
	s.MRUSize      = kstatInt(kstat, "mru_size")

	s.Hits             = kstatInt(kstat, "hits")
	s.Misses           = kstatInt(kstat, "misses")
	s.DemandDataHits   = kstatInt(kstat, "demand_data_hits")
	s.DemandMetaHits   = kstatInt(kstat, "demand_metadata_hits")
	s.PrefetchDataHits = kstatInt(kstat, "prefetch_data_hits")

	s.EvictedMFU = kstatInt(kstat, "mfu_ghost_hits")
	s.EvictedMRU = kstatInt(kstat, "mru_ghost_hits")

	s.L2Hits   = kstatInt(kstat, "l2_hits")
	s.L2Misses = kstatInt(kstat, "l2_misses")
	s.L2Size   = kstatInt(kstat, "l2_size")

	total := s.Hits + s.Misses
	if total > 0 {
		s.HitRatio = float64(s.Hits) / float64(total) * 100
	}

	// Read kernel tunable parameters.
	s.ParamARCMax = readSysfsInt64(arcParamDir + "zfs_arc_max")
	s.ParamARCMin = readSysfsInt64(arcParamDir + "zfs_arc_min")

	// If sysfs params are 0 (not overridden), fall back to the live ARC value.
	if s.ParamARCMax == 0 {
		s.ParamARCMax = s.ARCMax
	}
	if s.ParamARCMin == 0 {
		s.ParamARCMin = s.ARCMin
	}

	return s, nil
}

// SetARCParams applies new ARC size limits immediately via sysfs and persists
// them across reboots by writing /etc/modprobe.d/zfs.conf.
//
// arcMax and arcMin are in bytes; pass 0 to leave a parameter unchanged.
func SetARCParams(arcMax, arcMin int64) error {
	if arcMax != 0 {
		if err := writeSysfsInt64(arcParamDir+"zfs_arc_max", arcMax); err != nil {
			return fmt.Errorf("set zfs_arc_max: %w", err)
		}
	}
	if arcMin != 0 {
		if err := writeSysfsInt64(arcParamDir+"zfs_arc_min", arcMin); err != nil {
			return fmt.Errorf("set zfs_arc_min: %w", err)
		}
	}
	return persistARCModprobe(arcMax, arcMin)
}

// persistARCModprobe writes (or updates) the ZFSNAS-managed block in
// /etc/modprobe.d/zfs.conf so that ARC parameters survive a reboot.
func persistARCModprobe(arcMax, arcMin int64) error {
	const begin = "# BEGIN ZFSNAS ARC"
	const end   = "# END ZFSNAS ARC"

	// Read the current file (if it exists).
	existing, err := os.ReadFile(modprobePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", modprobePath, err)
	}

	// Strip the existing managed block if present.
	content := string(existing)
	if i := strings.Index(content, begin); i >= 0 {
		if j := strings.Index(content, end); j >= 0 {
			content = content[:i] + content[j+len(end):]
			content = strings.TrimRight(content, "\n") + "\n"
		}
	}

	// Build the new managed block.
	var block strings.Builder
	block.WriteString(begin + "\n")
	if arcMax != 0 {
		block.WriteString(fmt.Sprintf("options zfs zfs_arc_max=%d\n", arcMax))
	}
	if arcMin != 0 {
		block.WriteString(fmt.Sprintf("options zfs zfs_arc_min=%d\n", arcMin))
	}
	block.WriteString(end + "\n")

	content = strings.TrimRight(content, "\n") + "\n" + block.String()

	// Write via sudo tee since /etc/modprobe.d/ requires root.
	cmd := exec.Command("sudo", "tee", modprobePath)
	cmd.Stdin = bytes.NewBufferString(content)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("write %s: %w: %s", modprobePath, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// readKstat parses a Linux kstat file (3-column: name type value) into a map.
func readKstat(path string) (map[string]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	m := make(map[string]int64)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		// Skip header lines.
		if strings.HasPrefix(line, "//") || strings.HasPrefix(line, "name") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 3 {
			continue
		}
		v, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			continue
		}
		m[parts[0]] = v
	}
	return m, scanner.Err()
}

func kstatInt(m map[string]int64, key string) int64 {
	return m[key]
}

func readSysfsInt64(path string) int64 {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func writeSysfsInt64(path string, v int64) error {
	// sysfs requires root; pipe through sudo tee.
	cmd := exec.Command("sudo", "tee", path)
	cmd.Stdin = bytes.NewBufferString(strconv.FormatInt(v, 10) + "\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
