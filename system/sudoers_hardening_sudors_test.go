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
	// (The ZFSNAS_VMSETUP netplan→ifupdown migration entries — ip/mv/cat/tee/rm
	// against /etc/netplan, /etc/network, /run/systemd — were removed in v6.6.16:
	// virtualization install now runs under full "sudo all", so those one-shot
	// commands no longer have hardened sudoers rules.)
	bad := []string{
		"/usr/bin/cat /proc/*/smaps_rollup",
		"/usr/bin/journalctl --since=*",
		// ZFSNAS_SYNCOID backup-mount commands (prefix before `*`).
		"/usr/bin/mkdir -p /tmp/znas-bkup-mount-*",
		"/usr/bin/rmdir /tmp/znas-bkup-mount-*",
		"/usr/bin/umount /tmp/znas-bkup-mount-*",
		"/usr/bin/cat /tmp/znas-bkup-mount-*",
		"/usr/bin/tee /tmp/znas-bkup-mount-*",
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
		"/usr/bin/mkdir -p *",
		"/usr/bin/rmdir *",
		"/usr/bin/umount *",
	}
	for _, w := range want {
		if !strings.Contains(rendered, w) {
			t.Errorf("rendered sudoers missing expected widened form: %q", w)
		}
	}
}

// TestApplySudoRSSubstitutionsAlwaysWidens verifies the rewrite is applied
// unconditionally (regardless of the host's sudo flavor), so ZNAS always writes
// a sudoers file that loads on both classic sudo and sudo-rs.
func TestApplySudoRSSubstitutionsAlwaysWidens(t *testing.T) {
	in := "/usr/bin/cat /proc/*/smaps_rollup\n/usr/bin/journalctl --since=*\n"
	got := applySudoRSSubstitutions(in)
	if strings.Contains(got, "/usr/bin/cat /proc/*/smaps_rollup") || strings.Contains(got, "/usr/bin/journalctl --since=*") {
		t.Errorf("expected sudo-rs-incompatible forms to be widened, got %q", got)
	}
	if !strings.Contains(got, "/usr/bin/cat *") || !strings.Contains(got, "/usr/bin/journalctl *") {
		t.Errorf("expected widened forms present, got %q", got)
	}
}
