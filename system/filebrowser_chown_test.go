package system

import "testing"

// TestUserGroupExistsAcceptsNumericID guards the File Browser "Custom ID"
// path: a bare unsigned integer must be treated as a valid UID/GID without
// requiring a matching /etc/passwd or /etc/group entry. chown(1) accepts
// numeric IDs and the sudoers `chown *` rule already covers them; the only
// thing that has to give is the host-side whitelist. Regression test for
// 6.5.15.
func TestUserGroupExistsAcceptsNumericID(t *testing.T) {
	for _, id := range []string{"0", "1000", "4294967295"} {
		if !userExists(id) {
			t.Errorf("userExists(%q) = false, want true (numeric UID must be accepted)", id)
		}
		if !groupExists(id) {
			t.Errorf("groupExists(%q) = false, want true (numeric GID must be accepted)", id)
		}
	}
}

// TestUserGroupExistsRejectsBogusNumericForms makes sure the numeric path
// stays narrow: signed values, decimals, hex, whitespace, and 11-digit
// numbers (exceeds uint32 max) must NOT be silently accepted as IDs. Any
// of these would otherwise reach chown's argv, and even though the sudoers
// rule wouldn't escape they still want to be rejected at the validation
// layer for good error messages.
func TestUserGroupExistsRejectsBogusNumericForms(t *testing.T) {
	for _, id := range []string{
		"-1",          // negative
		" 1000",       // leading space
		"1000 ",       // trailing space
		"1000\n",      // trailing newline
		"0x10",        // hex
		"1e3",         // scientific
		"1000.0",      // decimal
		"42949672950", // 11 digits, > uint32 max — regex rejects on length
	} {
		if userExists(id) {
			// userExists may also return true if the literal string happens
			// to be a real /etc/passwd entry on the test host (extremely
			// unlikely for any of these). If that ever happens the test
			// host has bigger problems than this rule.
			t.Errorf("userExists(%q) = true, want false", id)
		}
		if groupExists(id) {
			t.Errorf("groupExists(%q) = true, want false", id)
		}
	}
}
