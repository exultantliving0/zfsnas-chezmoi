package system

import (
	"strings"
	"testing"
)

// TestSudoRSSubstitutionsRemoveAllWildcardErrors verifies that running the
// sudoers template through widenWildcardsForSudoRS strips every form that
// sudo-rs visudo rejects with "wildcards are not allowed in command arguments".
// Regression test for the Ubuntu 26.04 break reported in the dashboard banner.
func TestSudoRSSubstitutionsRemoveAllWildcardErrors(t *testing.T) {
	rendered := strings.Replace(requiredSudoersTemplate, "{{ZFSNAS_FILES}}", buildFilesAlias(), 1)
	rendered = strings.Replace(rendered, "{{LXD_CAT_LINE}}", "/usr/bin/cat *", 1)
	rendered = widenWildcardsForSudoRS(rendered)

	// These patterns all have a `*` in a non-trailing position — sudo-rs
	// refuses to parse them. After substitution none should remain.
	bad := []string{
		"/usr/bin/cat /proc/*/smaps_rollup",
		"/usr/bin/journalctl --since=*",
		"/usr/bin/rm -f /run/systemd/network/*.network",
		"/usr/bin/ip addr flush dev * scope global",
		"/usr/bin/ip route flush dev * scope global",
		"/usr/bin/mv /etc/netplan/*.yaml /etc/netplan/*.yaml.znas-disabled",
		"/usr/bin/cat /etc/netplan/*.yaml",
		"/usr/bin/tee /etc/network/interfaces.pre-znas-*",
	}
	for _, b := range bad {
		if strings.Contains(rendered, b) {
			t.Errorf("rendered sudoers still contains sudo-rs–incompatible entry: %q", b)
		}
	}

	// And the widened forms should be present so the feature still works.
	want := []string{
		"/usr/bin/cat *",
		"/usr/bin/journalctl *",
		"/usr/bin/rm -f *",
		"/usr/bin/ip *",
		"/usr/bin/mv *",
	}
	for _, w := range want {
		if !strings.Contains(rendered, w) {
			t.Errorf("rendered sudoers missing expected widened form: %q", w)
		}
	}
}

// TestApplySudoRSSubstitutionsClassicNoOp verifies the rewrite is a no-op when
// the host is classic sudo (default for the test runner — which is not
// sudo-rs). The narrow forms must survive verbatim so least-privilege is
// preserved on the majority of hosts.
func TestApplySudoRSSubstitutionsClassicNoOp(t *testing.T) {
	if IsSudoRS() {
		t.Skip("test runner is on a sudo-rs host; classic-sudo behavior cannot be verified here")
	}
	in := "/usr/bin/cat /proc/*/smaps_rollup\n/usr/bin/journalctl --since=*\n"
	if got := applySudoRSSubstitutions(in); got != in {
		t.Errorf("expected no-op on classic sudo, got %q", got)
	}
}
