package system

import (
	"strings"
	"testing"
)

// TestBridgeStanzaTailKernel6 — historical shape, used on every Linux
// version ZNAS shipped on before May 2026. Must include `hwaddress ether`
// for MAC pinning and `bridge-vlan-aware yes` + `bridge-vids 2-4094` for
// per-port VLAN filtering. The combination has worked since the migration
// landed in v6.5.6 and is verified on a Debian 13 test host (kernel
// 6.12).
func TestBridgeStanzaTailKernel6(t *testing.T) {
	c := bridgeCandidate{NIC: "enp2s0f0", MAC: "f4:e9:d4:99:41:a0", Bridge: "vmbr0"}
	got := bridgeKernelStanzaTail(c, false)
	wants := []string{
		"hwaddress ether f4:e9:d4:99:41:a0",
		"bridge_ports enp2s0f0",
		"bridge_stp off",
		"bridge_fd 0",
		"bridge-vlan-aware yes",
		"bridge-vids 2-4094",
	}
	for _, w := range wants {
		if !strings.Contains(got, w) {
			t.Errorf("kernel ≤ 6 stanza missing %q in:\n%s", w, got)
		}
	}
	if strings.Contains(got, "pre-up") {
		t.Errorf("kernel ≤ 6 stanza must not use pre-up form (regression):\n%s", got)
	}
}

// TestBridgeStanzaTailKernel7 — production-incident regression test for
// an Ubuntu 26.04 test host (kernel 7.0.0-15-generic, May 2026).
// Three things MUST be true on this kernel:
//
//   - No `hwaddress ether` directive (kernel rejects it with EADDRINUSE
//     when the slaved port has the same MAC; vmbr0 fails to come up).
//   - No `bridge-vlan-aware yes` (enabling vlan_filtering on a bridge
//     with an untagged-traffic-carrying port drops VID 1).
//   - No `bridge-vids 2-4094` (same regression — the explicit VID add
//     is what triggers the drop of VID 1).
//
// MAC pinning, when needed, must be expressed as a `pre-up` ip-link
// invocation, which runs before bridge_ports adds the slave and avoids
// the kernel-side conflict.
func TestBridgeStanzaTailKernel7(t *testing.T) {
	c := bridgeCandidate{NIC: "enp2s0f0", MAC: "f4:e9:d4:99:41:a0", Bridge: "vmbr0"}
	got := bridgeKernelStanzaTail(c, true)
	mustHave := []string{
		"pre-up /usr/sbin/ip link set vmbr0 address f4:e9:d4:99:41:a0",
		"bridge_ports enp2s0f0",
		"bridge_stp off",
		"bridge_fd 0",
	}
	for _, w := range mustHave {
		if !strings.Contains(got, w) {
			t.Errorf("kernel ≥ 7 stanza missing %q in:\n%s", w, got)
		}
	}
	mustNotHave := []string{
		"hwaddress ether",
		"bridge-vlan-aware",
		"bridge-vids",
	}
	for _, w := range mustNotHave {
		if strings.Contains(got, w) {
			t.Errorf("kernel ≥ 7 stanza must NOT include %q (regression — locks host out):\n%s", w, got)
		}
	}
}

// TestBridgeStanzaTailNoMAC — when the source NIC's MAC isn't known
// (rare; only happens when ip-addr capture fails), neither the
// hwaddress nor the pre-up form should be emitted.
func TestBridgeStanzaTailNoMAC(t *testing.T) {
	c := bridgeCandidate{NIC: "enp2s0f0", Bridge: "vmbr0"}
	for _, k7 := range []bool{false, true} {
		got := bridgeKernelStanzaTail(c, k7)
		if strings.Contains(got, "hwaddress") || strings.Contains(got, "pre-up") {
			t.Errorf("c.MAC=\"\" k7=%v should emit no MAC pin, got:\n%s", k7, got)
		}
	}
}

// TestKernelGTE7Detector exercises the version detector against the
// running kernel — strictly a smoke test, since the actual answer
// depends on the test host. Just makes sure the function doesn't
// panic and returns something consistent on repeat calls (it's
// memoized via sync.Once).
func TestKernelGTE7DetectorSmoke(t *testing.T) {
	a := kernelGTE7()
	b := kernelGTE7()
	if a != b {
		t.Errorf("kernelGTE7 not memoized: first=%v second=%v", a, b)
	}
}
