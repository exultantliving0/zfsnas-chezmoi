package system

import "testing"

// TestNormalizeTagRejectsUnsafeChars locks in the XSS hardening: tag names that
// contain HTML/attribute-context-dangerous characters must be rejected (return ""),
// while ordinary names (incl. spaces, accents, dot/dash/underscore) pass.
func TestNormalizeTagRejectsUnsafeChars(t *testing.T) {
	rejected := []string{
		`a'><img src=x onerror=alert(1)>`, // the proven payload
		`x'`,
		`a"b`,
		`a<b`,
		`a>b`,
		`a&b`,
		"a`b",
		"a,b",    // comma (storage separator)
		"a\tb",   // control char
		"a\x00b", // NUL
	}
	for _, in := range rejected {
		if got := normalizeTag(in); got != "" {
			t.Errorf("normalizeTag(%q) = %q; want \"\" (should be rejected)", in, got)
		}
	}

	ok := map[string]string{
		"Prod":       "prod",  // lowercased
		"  infra  ":  "infra", // trimmed
		"vl500":      "vl500",
		"web server": "web server", // space allowed
		"db-1":       "db-1",
		"node_2":     "node_2",
		"v1.2":       "v1.2",
		"café":       "café", // unicode letters allowed
	}
	for in, want := range ok {
		if got := normalizeTag(in); got != want {
			t.Errorf("normalizeTag(%q) = %q; want %q", in, got, want)
		}
	}
}
