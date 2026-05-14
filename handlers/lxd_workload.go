package handlers

import (
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"zfsnas/internal/capacityrrd"
	"zfsnas/system"
)

// lxd_workload.go — backs the "Workload" tab on the Performance page.
// One endpoint aggregates the existing per-instance RRDs into four
// per-metric maps so the frontend can pick the top-10 contributors and
// render stacked area charts without making N HTTP requests.
//
// No new RRD storage is introduced: this is a query-time aggregator
// over the same DBs already written by the LXD metrics collector.

// physicalDiskRe matches whole block devices the guest sees as real
// hardware (sda, vda, nvme0n1, …) plus the Incus-side device names that
// appear for VMs without a guest agent (`dev-incus_root`, etc.).
// Partitions, dm-*, loop\d+, and QEMU's `/machine/system.flash*` firmware
// blockers are excluded so summed disk I/O isn't double-counted or
// padded with idle pseudo-devices.
var physicalDiskRe = regexp.MustCompile(`^(sd[a-z]+|vd[a-z]+|xvd[a-z]+|nvme\d+n\d+|dev-(?:incus|lxd)_[A-Za-z0-9._-]+)$`)

// HandleLXDWorkloadPerf serves the per-instance RRD samples for the
// metrics shown on the Workload tab — cpu, mem, net_rx, net_tx, disk_r,
// disk_w — keyed by instance name. Network and disk samples are summed
// across all relevant devices per instance before being returned (so a
// VM with three zvols contributes a single disk_w line, not three).
//
// GET /api/incus/workload-perf?tier=0&since=<unix>
//
// Response:
//
//	{
//	  "tier":      0,
//	  "instances": ["vm1", "vm2", …],
//	  "metrics":   {
//	    "cpu":     { "vm1": [{ts,avg,min,max,n}, …], … },
//	    "mem":     { "vm1": […], … },
//	    "net_rx":  { … },
//	    "net_tx":  { … },
//	    "disk_r":  { … },
//	    "disk_w":  { … },
//	  }
//	}
func HandleLXDWorkloadPerf(w http.ResponseWriter, r *http.Request) {
	tier, _ := strconv.Atoi(r.URL.Query().Get("tier"))
	if tier < 0 || tier > 2 {
		tier = 0
	}
	var since int64
	if s := r.URL.Query().Get("since"); s != "" {
		since, _ = strconv.ParseInt(s, 10, 64)
	}

	instances := system.ListLXDMetricInstances()
	metrics := map[string]map[string][]capacityrrd.CapSample{
		"cpu":    {},
		"mem":    {},
		"net_rx": {},
		"net_tx": {},
		"disk_r": {},
		"disk_w": {},
	}

	for _, name := range instances {
		db := system.GetLXDInstanceMetricsDB(name)
		if db == nil {
			continue
		}
		keys := db.Keys()

		// Single-series gauges/rates.
		if hasKey(keys, "cpu") {
			metrics["cpu"][name] = querySamples(db, tier, "cpu", since)
		}
		if hasKey(keys, "mem") {
			metrics["mem"][name] = querySamples(db, tier, "mem", since)
		}

		// Network: sum across every device matching net_rx:* / net_tx:*.
		// No physical-NIC filter on the server side — we'd need the guest's
		// idea of "physical" which isn't recoverable from the metric name
		// alone. The frontend's _isPhysicalIface filter happens to allow
		// every label Incus typically emits anyway (enp*, ens*, eth*).
		if rx := sumKeys(db, tier, keys, "net_rx:", nil, since); rx != nil {
			metrics["net_rx"][name] = rx
		}
		if tx := sumKeys(db, tier, keys, "net_tx:", nil, since); tx != nil {
			metrics["net_tx"][name] = tx
		}

		// Disk: only sum physical-disk devices so we don't double-count
		// the same I/O across sda + sda1 + dm-0.
		if rd := sumKeys(db, tier, keys, "disk_r:", physicalDiskRe, since); rd != nil {
			metrics["disk_r"][name] = rd
		}
		if wr := sumKeys(db, tier, keys, "disk_w:", physicalDiskRe, since); wr != nil {
			metrics["disk_w"][name] = wr
		}
	}

	jsonOK(w, map[string]any{
		"tier":      tier,
		"instances": instances,
		"metrics":   metrics,
	})
}

func hasKey(keys []string, want string) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}

// querySamples returns the samples for one series, filtered to those at or
// after `since` (0 = all). Mirrors the cut logic in HandleLXDInstancePerf.
func querySamples(db *capacityrrd.DB, tier int, key string, since int64) []capacityrrd.CapSample {
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
	return samples
}

// sumKeys aggregates samples across every series whose key starts with
// `prefix`. When `deviceFilter` is non-nil it must match the part of the
// key after the prefix. Samples are aligned by timestamp; any timestamp
// present in at least one series ends up in the result with summed avg /
// min / max / n values. Returns nil when no matching series exists.
func sumKeys(db *capacityrrd.DB, tier int, keys []string, prefix string, deviceFilter *regexp.Regexp, since int64) []capacityrrd.CapSample {
	type accum struct {
		min, avg, max float64
		n             int
	}
	bag := map[int64]*accum{}
	matched := 0
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if deviceFilter != nil && !deviceFilter.MatchString(k[len(prefix):]) {
			continue
		}
		matched++
		for _, s := range querySamples(db, tier, k, since) {
			a := bag[s.TS]
			if a == nil {
				a = &accum{}
				bag[s.TS] = a
			}
			a.min += s.Min
			a.avg += s.Avg
			a.max += s.Max
			if s.N > a.n {
				a.n = s.N
			}
		}
	}
	if matched == 0 {
		return nil
	}
	ts := make([]int64, 0, len(bag))
	for k := range bag {
		ts = append(ts, k)
	}
	sort.Slice(ts, func(i, j int) bool { return ts[i] < ts[j] })
	out := make([]capacityrrd.CapSample, 0, len(ts))
	for _, t := range ts {
		a := bag[t]
		out = append(out, capacityrrd.CapSample{TS: t, Min: a.min, Avg: a.avg, Max: a.max, N: a.n})
	}
	return out
}
