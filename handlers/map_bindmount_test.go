package handlers

import "testing"

// TestResolveByPathBindMount covers the path→dataset resolution used both for
// SMB/NFS share paths and for container bind-mount sources (a disk device whose
// source is an absolute host path on a dataset).
func TestResolveByPathBindMount(t *testing.T) {
	dsByMount := map[string]string{
		"/tank":          "ds:tank",
		"/tank/media":    "ds:tank/media",
		"/tank/media/4k": "ds:tank/media/4k",
		"/srv":           "ds:srv",
	}
	cases := []struct {
		name string
		path string
		want string
	}{
		{"exact mountpoint", "/tank/media", "ds:tank/media"},
		{"subdir of dataset", "/tank/media/movies", "ds:tank/media"},
		{"longest prefix wins", "/tank/media/4k/films", "ds:tank/media/4k"},
		{"falls back to parent dataset", "/tank/other", "ds:tank"},
		{"trailing slash tolerated", "/tank/media/", "ds:tank/media"},
		{"unrelated host dir", "/var/lib/docker", ""},
		{"dev passthrough", "/dev/sda", ""},
		{"empty", "", ""},
	}
	for _, c := range cases {
		if got := resolveByPath(dsByMount, c.path); got != c.want {
			t.Errorf("%s: resolveByPath(%q) = %q, want %q", c.name, c.path, got, c.want)
		}
	}
}
