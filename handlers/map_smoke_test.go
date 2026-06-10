package handlers

import (
	"encoding/json"
	"testing"

	"zfsnas/internal/config"
	"zfsnas/system"
)

// TestBuildMapTopologySmoke ensures the topology builder never panics and always
// returns a JSON-marshalable document, even when every underlying system probe
// fails (no ZFS / no Samba / no sudo) — the normal case in CI.
func TestBuildMapTopologySmoke(t *testing.T) {
	cfg := &config.AppConfig{}
	top := buildMapTopology(cfg)

	if top.TS == 0 {
		t.Fatalf("expected a timestamp on the topology")
	}
	// Slices must be non-nil-safe to marshal; encoding/json handles nil slices,
	// but we assert the whole doc round-trips.
	if _, err := json.Marshal(top); err != nil {
		t.Fatalf("topology is not JSON-marshalable: %v", err)
	}
	// The Networking Layer section must always marshal too (it's nil-safe when
	// virtualization is unavailable in CI).
	if _, err := json.Marshal(top.Net); err != nil {
		t.Fatalf("net section is not JSON-marshalable: %v", err)
	}
}

// TestParseDockerPublishedPorts verifies the 0.0.0.0/[::] host-port extraction
// used to label docker nodes on the Networking Layer.
func TestParseDockerPublishedPorts(t *testing.T) {
	cases := []struct {
		in   string
		want int // number of unique ports expected
	}{
		{"", 0},
		{"80/tcp", 0}, // not published on host
		{"0.0.0.0:8091->80/tcp", 1},
		{"0.0.0.0:8091->80/tcp, [::]:8091->80/tcp", 1},                 // dual-stack collapses
		{"0.0.0.0:53->53/udp, 0.0.0.0:80->80/tcp, [::]:80->80/tcp", 2}, // tcp80 + udp53
	}
	for _, c := range cases {
		got := system.ParseDockerPublishedPorts(c.in)
		if len(got) != c.want {
			t.Errorf("ParseDockerPublishedPorts(%q) = %d ports, want %d (%+v)", c.in, len(got), c.want, got)
		}
	}
	// Spot-check proto/port decoding.
	ports := system.ParseDockerPublishedPorts("0.0.0.0:53->53/udp")
	if len(ports) != 1 || ports[0].Proto != "udp" || ports[0].Port != 53 {
		t.Errorf("expected udp:53, got %+v", ports)
	}
}
