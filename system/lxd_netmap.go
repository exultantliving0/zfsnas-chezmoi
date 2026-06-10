package system

import (
	"encoding/json"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Networking-layer topology data (v6.6.4)
//
// Feeds the Map's "Networking Layer": bridges, the instances attached to each
// bridge, per-NIC live bandwidth (derived from the incus metrics counters), and
// the docker containers running inside each instance with the TCP/UDP ports they
// publish on 0.0.0.0. All of this is read-only and best-effort — any piece that
// is unavailable (metrics off, docker not present) simply yields zero/empty.
// ─────────────────────────────────────────────────────────────────────────────

// LXDNetRate is one instance-NIC's live throughput in megabits per second.
type LXDNetRate struct {
	RxMbps float64 `json:"rx_mbps"`
	TxMbps float64 `json:"tx_mbps"`
}

// netRateSample caches the last counter reading for a single (instance, device)
// so a rate can be derived from the byte-counter delta on the next scrape.
type netRateSample struct {
	rx, tx float64
	at     time.Time
}

var (
	netRateMu   sync.Mutex
	netRatePrev = map[string]netRateSample{} // key "instance\x00device"
)

// LXDNetRates scrapes the incus metrics endpoint once and returns the live
// per-NIC throughput for every instance/device, keyed [instance][device].
// Rates are computed from the delta against the previous call's counters, so
// the first call after start (or after a counter reset) returns 0 for new keys.
func LXDNetRates() map[string]map[string]LXDNetRate {
	out := map[string]map[string]LXDNetRate{}
	body, err := fetchLXDMetricsBody()
	if err != nil {
		return out
	}
	samples := parsePromText(body)
	now := time.Now()

	// Collect current absolute counters per (instance, device).
	type cur struct{ rx, tx float64 }
	curByKey := map[string]*cur{}
	keyOf := func(inst, dev string) string { return inst + "\x00" + dev }
	get := func(inst, dev string) *cur {
		k := keyOf(inst, dev)
		c := curByKey[k]
		if c == nil {
			c = &cur{}
			curByKey[k] = c
		}
		return c
	}
	for _, s := range samples {
		inst := s.labels["name"]
		dev := s.labels["device"]
		if inst == "" || dev == "" {
			continue
		}
		switch s.metric {
		case "lxd_network_receive_bytes_total":
			get(inst, dev).rx = s.value
		case "lxd_network_transmit_bytes_total":
			get(inst, dev).tx = s.value
		}
	}

	netRateMu.Lock()
	defer netRateMu.Unlock()
	for k, c := range curByKey {
		prev, ok := netRatePrev[k]
		netRatePrev[k] = netRateSample{rx: c.rx, tx: c.tx, at: now}
		if !ok {
			continue
		}
		dt := now.Sub(prev.at).Seconds()
		if dt <= 0 {
			continue
		}
		rate := LXDNetRate{}
		if c.rx >= prev.rx {
			rate.RxMbps = (c.rx - prev.rx) * 8 / dt / 1e6
		}
		if c.tx >= prev.tx {
			rate.TxMbps = (c.tx - prev.tx) * 8 / dt / 1e6
		}
		parts := strings.SplitN(k, "\x00", 2)
		inst, dev := parts[0], parts[1]
		if out[inst] == nil {
			out[inst] = map[string]LXDNetRate{}
		}
		out[inst][dev] = rate
	}
	// Drop stale keys (instance/device gone) so the cache doesn't grow forever.
	for k, p := range netRatePrev {
		if now.Sub(p.at) > 2*time.Minute {
			delete(netRatePrev, k)
		}
	}
	return out
}

// LXDInstanceNICAttach is one instance↔bridge NIC attachment for the network map.
type LXDInstanceNICAttach struct {
	Instance string `json:"instance"`
	Device   string `json:"device"`
	Bridge   string `json:"bridge"`
}

// LXDAllInstanceNICs returns every instance's connected bridged NICs in a single
// `incus list` call (cheaper than one query per instance). Only NICs that
// resolve to a bridge parent/network are returned; disconnected NICs (which live
// only in user.disconnected_nics.* config) are omitted from the live map.
func LXDAllInstanceNICs() []LXDInstanceNICAttach {
	out, err := exec.Command("incus", "list", "--format", "json").Output()
	if err != nil {
		return nil
	}
	var raw []struct {
		Name            string                     `json:"name"`
		ExpandedDevices map[string]json.RawMessage `json:"expanded_devices"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil
	}
	var res []LXDInstanceNICAttach
	for _, inst := range raw {
		for devName, rawDev := range inst.ExpandedDevices {
			var fields map[string]json.RawMessage
			if json.Unmarshal(rawDev, &fields) != nil {
				continue
			}
			str := func(k string) string {
				var s string
				if v, ok := fields[k]; ok {
					json.Unmarshal(v, &s) //nolint:errcheck
				}
				return s
			}
			if str("type") != "nic" {
				continue
			}
			// A managed-network NIC carries "network"; a manual bridged NIC carries
			// "parent" (with nictype=bridged). Prefer network, fall back to parent.
			bridge := str("network")
			if bridge == "" && (str("nictype") == "bridged" || str("nictype") == "macvlan") {
				bridge = str("parent")
			}
			if bridge == "" {
				bridge = str("parent") // last resort
			}
			if bridge == "" {
				continue
			}
			res = append(res, LXDInstanceNICAttach{Instance: inst.Name, Device: devName, Bridge: bridge})
		}
	}
	return res
}

// reDockerPort matches one host-published port mapping in `docker ps` Ports text,
// e.g. "0.0.0.0:8091->80/tcp" or "[::]:53->53/udp". We only surface the HOST
// listen port (the left side) for services bound to all interfaces.
var reDockerPort = regexp.MustCompile(`(?:0\.0\.0\.0|\[::\]):(\d+)->\d+/(tcp|udp)`)

// LXDNetDockerPort is one TCP/UDP port a docker container publishes on 0.0.0.0.
type LXDNetDockerPort struct {
	Proto string `json:"proto"`
	Port  int    `json:"port"`
}

// ParseDockerPublishedPorts extracts the unique 0.0.0.0/[::] host-published
// ports from a `docker ps`-style Ports string. Dual-stack bindings (a port
// listed for both 0.0.0.0 and [::]) collapse to a single entry per proto.
func ParseDockerPublishedPorts(portsStr string) []LXDNetDockerPort {
	seen := map[string]bool{}
	var res []LXDNetDockerPort
	for _, m := range reDockerPort.FindAllStringSubmatch(portsStr, -1) {
		port, _ := strconv.Atoi(m[1])
		proto := m[2]
		key := proto + ":" + m[1]
		if port == 0 || seen[key] {
			continue
		}
		seen[key] = true
		res = append(res, LXDNetDockerPort{Proto: proto, Port: port})
	}
	return res
}
