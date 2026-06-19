package system

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"zfsnas/internal/config"
)

// VM/Container tagging (v6.6.19).
//
// Tags travel with the instances so a relayed peer's VM shows the same
// tags/colors regardless of which portal renders them. Therefore tag data
// lives on the Incus host, not in the home server's user config:
//
//   - Per-instance membership → Incus instance config key user.zfsnas.tags
//     (a comma-separated list of tag names).
//   - Tag → color registry     → config/vmtags.json on this host, a single
//     {name: "#rrggbb"} map. A tag's color is one shared property, so storing
//     it once makes a recolor an O(1) write instead of rewriting every
//     instance, and avoids "same tag, different color per VM" drift.

const tagRegistryFile = "vmtags.json"

// tagInstanceConfigKey is the Incus user.* key holding an instance's tags.
const tagInstanceConfigKey = "user.zfsnas.tags"

// MaxTagsPerInstance caps how many tags an instance may carry.
const MaxTagsPerInstance = 12

// maxTagNameLen caps a tag name's length.
const maxTagNameLen = 32

// tagPalette is the auto-assignment palette: theme-agnostic, distinct hues.
var tagPalette = []string{
	"#e5484d", "#f76808", "#f5d90a", "#46a758", "#12a594", "#0091ff",
	"#3e63dd", "#8e4ec6", "#d6409f", "#e54666", "#978365", "#6e7681",
}

var tagHexRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// tagRegMu guards read-modify-write cycles on the registry file so two
// concurrent SetInstanceTags calls can't clobber each other's new colors.
var tagRegMu sync.Mutex

// tagRegistryPath returns the absolute path to the color registry file.
func tagRegistryPath() string {
	return filepath.Join(config.Dir(), tagRegistryFile)
}

// LoadTagRegistry reads the tag→color map. A missing file yields an empty map.
func LoadTagRegistry() (map[string]string, error) {
	data, err := os.ReadFile(tagRegistryPath())
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	reg := map[string]string{}
	if err := json.Unmarshal(data, &reg); err != nil {
		// A corrupt registry shouldn't brick tagging — start fresh.
		return map[string]string{}, nil
	}
	return reg, nil
}

// saveTagRegistry writes the registry atomically (temp file + rename).
func saveTagRegistry(reg map[string]string) error {
	data, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	path := tagRegistryPath()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// normalizeTag trims, lowercases, and validates a single tag name.
// Lowercasing avoids near-duplicate clutter (Prod vs prod). Returns "" for
// names that are empty or contain a comma/control char after trimming.
func normalizeTag(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return ""
	}
	if strings.ContainsRune(name, ',') {
		return ""
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return ""
		}
	}
	if len(name) > maxTagNameLen {
		name = name[:maxTagNameLen]
	}
	return name
}

// normalizeTags cleans, de-dups (preserving order), and caps a tag list.
func normalizeTags(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, raw := range in {
		n := normalizeTag(raw)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
		if len(out) >= MaxTagsPerInstance {
			break
		}
	}
	return out
}

// autoTagColor picks a stable palette color from a tag name, so two users
// typing the same tag independently land on the same default.
func autoTagColor(name string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return tagPalette[int(h.Sum32())%len(tagPalette)]
}

// parseInstanceTags splits a raw user.zfsnas.tags value into clean names.
func parseInstanceTags(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	return normalizeTags(strings.Split(raw, ","))
}

// GetInstanceTags returns the tags currently set on an instance.
func GetInstanceTags(name string) ([]string, error) {
	out, err := exec.Command("incus", "config", "get", name, tagInstanceConfigKey).Output()
	if err != nil {
		return nil, err
	}
	return parseInstanceTags(string(out)), nil
}

// SetInstanceTags replaces the tag list on an instance and ensures every new
// tag name has a color in the registry.
func SetInstanceTags(name string, tags []string) error {
	clean := normalizeTags(tags)

	// Register colors for any tag the registry doesn't know yet.
	tagRegMu.Lock()
	reg, err := LoadTagRegistry()
	if err != nil {
		tagRegMu.Unlock()
		return err
	}
	dirty := false
	for _, t := range clean {
		if _, ok := reg[t]; !ok {
			reg[t] = autoTagColor(t)
			dirty = true
		}
	}
	if dirty {
		if err := saveTagRegistry(reg); err != nil {
			tagRegMu.Unlock()
			return err
		}
	}
	tagRegMu.Unlock()

	if len(clean) == 0 {
		// Unset the key entirely when no tags remain.
		_ = exec.Command("incus", "config", "unset", name, tagInstanceConfigKey).Run()
		return nil
	}
	joined := strings.Join(clean, ",")
	if out, err := exec.Command("incus", "config", "set", name, tagInstanceConfigKey+"="+joined).CombinedOutput(); err != nil {
		return fmt.Errorf("incus config set tags: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// SetTagColor sets a tag's color in the registry, which recolors that tag on
// every instance carrying it.
func SetTagColor(name, hexColor string) error {
	tag := normalizeTag(name)
	if tag == "" {
		return fmt.Errorf("invalid tag name")
	}
	if !tagHexRe.MatchString(hexColor) {
		return fmt.Errorf("invalid color %q (want #rrggbb)", hexColor)
	}
	tagRegMu.Lock()
	defer tagRegMu.Unlock()
	reg, err := LoadTagRegistry()
	if err != nil {
		return err
	}
	reg[tag] = strings.ToLower(hexColor)
	return saveTagRegistry(reg)
}

// TagRegistrySorted returns the registry with names sorted, for stable display.
func TagRegistrySorted() ([]string, map[string]string, error) {
	reg, err := LoadTagRegistry()
	if err != nil {
		return nil, nil, err
	}
	names := make([]string, 0, len(reg))
	for n := range reg {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, reg, nil
}
