package system

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// FileEntry describes one file or directory returned by ListDir.
type FileEntry struct {
	Name         string `json:"name"`
	IsDir        bool   `json:"is_dir"`
	SizeBytes    int64  `json:"size_bytes"`
	Permissions  string `json:"permissions"`  // e.g. "-rw-r--r--"
	ModeOctal    string `json:"mode_octal"`   // e.g. "644"
	Owner        string `json:"owner"`
	Group        string `json:"group"`
	ModifiedUnix int64  `json:"modified_unix"`
}

// FileBrowserResult is returned by the list API.
type FileBrowserResult struct {
	RootLabel  string      `json:"root_label"`
	Subpath    string      `json:"subpath"`
	CurrentDir *FileEntry  `json:"current_dir,omitempty"` // stats for the directory being listed
	Entries    []FileEntry `json:"entries"`
}

// ── ListDir ───────────────────────────────────────────────────────────────────

// ListDir reads the contents of root/subpath using sudo find so that directory
// permission restrictions on the zfsnas service account are bypassed.
// Entries are sorted: directories first, then files, both alphabetically.
func ListDir(root, subpath, rootLabel string) (*FileBrowserResult, error) {
	absPath, err := SafeJoin(root, subpath)
	if err != nil {
		return nil, err
	}

	// sudo find <path> -maxdepth 1 -mindepth 1
	//   -printf '%y\t%s\t%m\t%M\t%U\t%G\t%T@\t%P\n'
	// Fields: type | size | octal-mode | symbolic-mode | owner | group | mtime-epoch | name
	// %m gives the plain octal permissions (e.g. "755"), %M the symbolic string.
	// %T@ is mtime as Unix seconds with fractional part.
	// %P is the filename relative to the starting path (no leading slash).
	out, err := exec.Command("sudo", "find", absPath,
		"-maxdepth", "1", "-mindepth", "1",
		"-printf", "%y\t%s\t%m\t%M\t%U\t%G\t%T@\t%P\n",
	).Output()
	if err != nil {
		return nil, fmt.Errorf("failed to list directory: %w", err)
	}

	var result []FileEntry
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		// SplitN with n=8 so a filename containing tabs ends up as-is in parts[7].
		parts := strings.SplitN(line, "\t", 8)
		if len(parts) < 8 {
			continue
		}
		ftype, sizeStr, modeOctal, perms, owner, group, mtimeStr, name :=
			parts[0], parts[1], parts[2], parts[3], parts[4], parts[5], parts[6], parts[7]

		isDir := ftype == "d"

		var size int64
		if !isDir {
			size, _ = strconv.ParseInt(sizeStr, 10, 64)
		}

		// Normalise octal to exactly 3 digits ("755", "644", "000").
		octal := strings.TrimLeft(modeOctal, "0")
		if len(octal) < 3 {
			octal = strings.Repeat("0", 3-len(octal)) + octal
		}
		if len(octal) > 3 {
			octal = octal[len(octal)-3:]
		}

		var mtime float64
		mtime, _ = strconv.ParseFloat(mtimeStr, 64)

		result = append(result, FileEntry{
			Name:         name,
			IsDir:        isDir,
			SizeBytes:    size,
			Permissions:  perms,
			ModeOctal:    octal,
			Owner:        owner,
			Group:        group,
			ModifiedUnix: int64(mtime),
		})
	}

	// Sort: directories first, then files, both alphabetically.
	sort.Slice(result, func(i, j int) bool {
		if result[i].IsDir != result[j].IsDir {
			return result[i].IsDir
		}
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})

	// Stat the current directory itself (for the folder-level burger menu).
	var currentDir *FileEntry
	if dirOut, err2 := exec.Command("sudo", "find", absPath, "-maxdepth", "0",
		"-printf", "%y\t%s\t%m\t%M\t%U\t%G\t%T@\t%f\n").Output(); err2 == nil {
		line := strings.TrimRight(string(dirOut), "\n")
		if parts := strings.SplitN(line, "\t", 8); len(parts) == 8 {
			var sz int64
			sz, _ = strconv.ParseInt(parts[1], 10, 64)
			octal := strings.TrimLeft(parts[2], "0")
			if len(octal) < 3 {
				octal = strings.Repeat("0", 3-len(octal)) + octal
			}
			if len(octal) > 3 {
				octal = octal[len(octal)-3:]
			}
			var mtime float64
			mtime, _ = strconv.ParseFloat(parts[6], 64)
			currentDir = &FileEntry{
				Name:         parts[7],
				IsDir:        true,
				SizeBytes:    sz,
				Permissions:  parts[3],
				ModeOctal:    octal,
				Owner:        parts[4],
				Group:        parts[5],
				ModifiedUnix: int64(mtime),
			}
		}
	}

	return &FileBrowserResult{
		RootLabel:  rootLabel,
		Subpath:    subpath,
		CurrentDir: currentDir,
		Entries:    result,
	}, nil
}

// ── GetSystemUsersGroups ──────────────────────────────────────────────────────

// GetSystemUsersGroups reads /etc/passwd and /etc/group and returns sorted name lists.
func GetSystemUsersGroups() (users, groups []string, err error) {
	{
		f, ferr := os.Open("/etc/passwd")
		if ferr != nil {
			return nil, nil, ferr
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			parts := strings.Split(sc.Text(), ":")
			if len(parts) >= 1 && parts[0] != "" {
				users = append(users, parts[0])
			}
		}
	}
	{
		f, ferr := os.Open("/etc/group")
		if ferr != nil {
			return nil, nil, ferr
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			parts := strings.Split(sc.Text(), ":")
			if len(parts) >= 1 && parts[0] != "" {
				groups = append(groups, parts[0])
			}
		}
	}
	sort.Strings(users)
	sort.Strings(groups)
	return users, groups, nil
}

// ── userGroupExists ───────────────────────────────────────────────────────────

// userExists checks that owner is present in /etc/passwd.
func userExists(owner string) bool {
	f, err := os.Open("/etc/passwd")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Split(sc.Text(), ":")
		if len(parts) >= 1 && parts[0] == owner {
			return true
		}
	}
	return false
}

// groupExists checks that group is present in /etc/group.
func groupExists(group string) bool {
	f, err := os.Open("/etc/group")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		parts := strings.Split(sc.Text(), ":")
		if len(parts) >= 1 && parts[0] == group {
			return true
		}
	}
	return false
}

// ── ChownPath ─────────────────────────────────────────────────────────────────

// ChownPath runs: sudo chown [-R] owner:group <absPath>
func ChownPath(absPath, owner, group string, recursive bool) error {
	if !userExists(owner) {
		return fmt.Errorf("user %q not found in /etc/passwd", owner)
	}
	if !groupExists(group) {
		return fmt.Errorf("group %q not found in /etc/group", group)
	}
	args := []string{"chown"}
	if recursive {
		args = append(args, "-R")
	}
	args = append(args, owner+":"+group, absPath)
	out, err := exec.Command("sudo", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("chown failed: %w — %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ── ChmodPath ─────────────────────────────────────────────────────────────────

var validMode = regexp.MustCompile(`^[0-7]{3}$`)

// ChmodPath runs: sudo chmod [-R] mode <absPath>
func ChmodPath(absPath, mode string, recursive bool) error {
	if !validMode.MatchString(mode) {
		return fmt.Errorf("invalid mode %q: must be a 3-digit octal string (000–777)", mode)
	}
	args := []string{"chmod"}
	if recursive {
		args = append(args, "-R")
	}
	args = append(args, mode, absPath)
	out, err := exec.Command("sudo", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("chmod failed: %w — %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// ── ResolveKnownRoots ─────────────────────────────────────────────────────────

// ResolveKnownRoots builds the set of legal root paths from the current state:
// ZFS dataset mountpoints, SMB share paths, NFS share paths.
// map key = absolute path, value = human-readable label (e.g. "tank/media")
func ResolveKnownRoots(configDir string) (map[string]string, error) {
	roots := map[string]string{}

	// ZFS dataset mountpoints.
	datasets, err := ListAllDatasets()
	if err == nil {
		for _, d := range datasets {
			mp := d.Mountpoint
			if mp == "" || mp == "none" || mp == "-" || mp == "legacy" {
				continue
			}
			roots[mp] = d.Name
		}
	}

	// SMB share paths.
	smbShares, err := ListSMBShares(configDir)
	if err == nil {
		for _, s := range smbShares {
			if s.Path == "" {
				continue
			}
			label := s.Name
			if label == "" {
				label = s.Path
			}
			roots[s.Path] = "SMB: " + label
		}
	}

	// NFS share paths.
	nfsShares, err := ListNFSShares(configDir)
	if err == nil {
		for _, s := range nfsShares {
			if s.Path == "" {
				continue
			}
			label := s.Path
			roots[s.Path] = "NFS: " + label
		}
	}

	if len(roots) == 0 {
		return roots, fmt.Errorf("no known roots found")
	}
	return roots, nil
}

// ── ValidateRootToken ─────────────────────────────────────────────────────────

// ValidateRootToken decodes a base64url root token and checks it against the
// known roots map. Returns the absolute path and label, or an error.
func ValidateRootToken(token string, knownRoots map[string]string) (absPath, label string, err error) {
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", "", fmt.Errorf("invalid root token")
	}
	absPath = string(b)
	label, ok := knownRoots[absPath]
	if !ok {
		return "", "", fmt.Errorf("root path is not a known share or dataset mountpoint")
	}
	return absPath, label, nil
}

// ── SafeJoin ──────────────────────────────────────────────────────────────────

// SafeJoin joins root + subpath and verifies the result is still under root.
// Returns an error if the joined path escapes root (directory traversal attempt).
func SafeJoin(root, subpath string) (string, error) {
	if subpath == "" || subpath == "." {
		return filepath.Clean(root), nil
	}
	clean := filepath.Clean(subpath)
	if strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("invalid subpath: directory traversal not allowed")
	}
	joined := filepath.Join(root, clean)
	rootClean := filepath.Clean(root)
	if !strings.HasPrefix(joined, rootClean+string(filepath.Separator)) && joined != rootClean {
		return "", fmt.Errorf("invalid subpath: escapes root directory")
	}
	return joined, nil
}
