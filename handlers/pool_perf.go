package handlers

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"zfsnas/internal/capacityrrd"
	"zfsnas/system"
)

// HandleGetPoolPerfPools returns the list of pool names that have performance data.
func HandleGetPoolPerfPools(w http.ResponseWriter, r *http.Request) {
	db := system.GetPoolPerfDB()
	if db == nil {
		jsonOK(w, []string{})
		return
	}
	poolSet := make(map[string]bool)
	for _, key := range db.Keys() {
		// keys are: read:{pool}:{dev}
		if strings.HasPrefix(key, "read:") {
			parts := strings.SplitN(key, ":", 3)
			if len(parts) == 3 {
				poolSet[parts[1]] = true
			}
		}
	}
	pools := make([]string, 0, len(poolSet))
	for p := range poolSet {
		pools = append(pools, p)
	}
	sort.Strings(pools)
	jsonOK(w, pools)
}

// HandleGetPoolPerfOldest returns the Unix timestamp of the oldest Tier0 sample for a given pool.
// Query param: pool. Returns { oldest_ts: 0 } when no data exists yet.
func HandleGetPoolPerfOldest(w http.ResponseWriter, r *http.Request) {
	db := system.GetPoolPerfDB()
	if db == nil {
		jsonOK(w, map[string]int64{"oldest_ts": 0})
		return
	}
	pool := r.URL.Query().Get("pool")
	if pool == "" {
		jsonErr(w, http.StatusBadRequest, "pool parameter required")
		return
	}
	prefix := "read:" + pool + ":"
	var oldest int64
	for _, key := range db.Keys() {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		samples := db.Query(0, key) // tier 0
		if len(samples) > 0 {
			ts := samples[0].TS
			if oldest == 0 || ts < oldest {
				oldest = ts
			}
		}
	}
	jsonOK(w, map[string]int64{"oldest_ts": oldest})
}

// HandleGetPoolPerfData returns multi-tier per-device disk performance RRD data for one pool.
// Query params: pool, tier (0=5-min/week, 1=30-min/month, 2=daily/5yr), since (unix ts)
// Response: { pool, tier, devices: ["sda","sdb",...], series: { "read:{pool}:{dev}": [...], ... } }
func HandleGetPoolPerfData(w http.ResponseWriter, r *http.Request) {
	db := system.GetPoolPerfDB()
	if db == nil {
		jsonErr(w, http.StatusServiceUnavailable, "pool perf collector not ready")
		return
	}

	pool := r.URL.Query().Get("pool")
	if pool == "" {
		jsonErr(w, http.StatusBadRequest, "pool parameter required")
		return
	}

	tier, _ := strconv.Atoi(r.URL.Query().Get("tier"))
	if tier < 0 || tier > 2 {
		tier = 0
	}

	var since int64
	if s := r.URL.Query().Get("since"); s != "" {
		since, _ = strconv.ParseInt(s, 10, 64)
	}

	// Discover devices for this pool from existing series keys.
	devSet := make(map[string]bool)
	prefix := "read:" + pool + ":"
	for _, key := range db.Keys() {
		if strings.HasPrefix(key, prefix) {
			dev := strings.TrimPrefix(key, prefix)
			if dev != "" {
				devSet[dev] = true
			}
		}
	}
	devices := make([]string, 0, len(devSet))
	for d := range devSet {
		devices = append(devices, d)
	}
	sort.Strings(devices)

	// Build the full key list: read/write/busy × device.
	var keys []string
	for _, dev := range devices {
		keys = append(keys,
			"read:"+pool+":"+dev,
			"write:"+pool+":"+dev,
			"busy:"+pool+":"+dev,
		)
	}

	result := make(map[string][]capacityrrd.CapSample, len(keys))
	for _, key := range keys {
		samples := db.Query(tier, key)
		if since > 0 {
			cut := len(samples)
			for i, s := range samples {
				if s.TS >= since {
					cut = i
					break
				}
			}
			samples = samples[cut:]
		}
		if samples == nil {
			samples = []capacityrrd.CapSample{}
		}
		result[key] = samples
	}

	jsonOK(w, map[string]interface{}{
		"pool":    pool,
		"tier":    tier,
		"devices": devices,
		"series":  result,
	})
}
