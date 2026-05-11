package system

import (
	"testing"
)

// TestParsePromTextNormalizesIncusPrefix — Incus 6.x renamed the
// Prometheus metric prefix from `lxd_*` to `incus_*` after the Canonical
// LXD → Incus fork. Ubuntu 26.04 ships Incus 6.0.5 and emits e.g.
// `incus_cpu_seconds_total`. The collector's switch cases match `lxd_*`,
// so without normalization at parse time, every sample on Incus 6.x was
// silently dropped and per-instance graphs stayed empty forever.
//
// Pin the normalization rule: any metric starting with `incus_` is
// rewritten to `lxd_` so the rest of the collector code (this file and
// lxd_global_config.go) can keep its single canonical name set.
func TestParsePromTextNormalizesIncusPrefix(t *testing.T) {
	body := `# HELP incus_cpu_seconds_total CPU seconds.
# TYPE incus_cpu_seconds_total counter
incus_cpu_seconds_total{cpu="0",mode="user",name="mediaserver",project="default",type="virtual-machine"} 12.34
incus_memory_Active_anon_bytes{name="mediaserver",project="default",type="virtual-machine"} 7321260032
incus_network_receive_bytes_total{device="eth0",name="mediaserver",project="default",type="virtual-machine"} 1024
`
	samples := parsePromText(body)
	if len(samples) != 3 {
		t.Fatalf("expected 3 samples, got %d", len(samples))
	}
	gotMetrics := map[string]bool{}
	for _, s := range samples {
		gotMetrics[s.metric] = true
	}
	for _, want := range []string{"lxd_cpu_seconds_total", "lxd_memory_Active_anon_bytes", "lxd_network_receive_bytes_total"} {
		if !gotMetrics[want] {
			t.Errorf("expected normalized metric %q, got names: %v", want, gotMetrics)
		}
	}
}

// TestParsePromTextPreservesLxdPrefix — older LXD installations (and any
// future Canonical-branded build) still emit `lxd_*` directly. The
// normalization rule must only rewrite `incus_*`; existing `lxd_*` names
// pass through verbatim so we don't accidentally double-process or strip
// legitimate samples on those hosts.
func TestParsePromTextPreservesLxdPrefix(t *testing.T) {
	body := `lxd_cpu_seconds_total{name="ct1"} 5.0
lxd_memory_MemTotal_bytes{name="ct1"} 1073741824
`
	samples := parsePromText(body)
	if len(samples) != 2 {
		t.Fatalf("expected 2 samples, got %d", len(samples))
	}
	if samples[0].metric != "lxd_cpu_seconds_total" {
		t.Errorf("lxd_* prefix mangled: got %q", samples[0].metric)
	}
	if samples[1].metric != "lxd_memory_MemTotal_bytes" {
		t.Errorf("lxd_* prefix mangled: got %q", samples[1].metric)
	}
}

// TestParsePromTextNormalizationCoversCollectorSwitchCases — every metric
// name the collector switches on in scrapeLXDMetricsOnce must have a
// normalized counterpart that successfully matches. Drift check: if
// someone adds a new `case "lxd_..."` in the collector but forgets that
// the Incus-side name is `incus_...`, this test still passes (because we
// normalize), and a real production run on Incus 6.x picks it up too.
// Conversely, if someone changes the normalization rule, the canonical
// name must remain `lxd_*` so the switch cases still match.
func TestParsePromTextNormalizationCoversCollectorSwitchCases(t *testing.T) {
	cases := []string{
		"incus_cpu_seconds_total",
		"incus_memory_Active_anon_bytes",
		"incus_memory_MemAvailable_bytes",
		"incus_memory_MemTotal_bytes",
		"incus_network_receive_bytes_total",
		"incus_network_transmit_bytes_total",
		"incus_disk_read_bytes_total",
		"incus_disk_written_bytes_total",
	}
	for _, in := range cases {
		body := in + `{name="x"} 1.0` + "\n"
		samples := parsePromText(body)
		if len(samples) != 1 {
			t.Fatalf("%s: expected 1 sample, got %d", in, len(samples))
		}
		want := "lxd_" + in[len("incus_"):]
		if samples[0].metric != want {
			t.Errorf("%s normalized to %q, want %q", in, samples[0].metric, want)
		}
	}
}
