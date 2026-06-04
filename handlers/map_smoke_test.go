package handlers

import (
	"encoding/json"
	"testing"

	"zfsnas/internal/config"
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
}
