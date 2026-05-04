package system

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// zfsResolvedMountpoint returns the actual kernel mount point for a ZFS
// dataset, walking up the hierarchy until it finds a mounted ancestor.
// LXD storage datasets (e.g. "tank/lxd-base") are never mounted themselves;
// in that case we use the root pool's mount point.
func zfsResolvedMountpoint(dataset string) (string, error) {
	// Walk from the given dataset up to the root pool looking for a mounted path.
	current := dataset
	for current != "" {
		mp, err := zfsOneMountpoint(current)
		if err == nil && filepath.IsAbs(mp) {
			return mp, nil
		}
		// Go up one level (strip last component).
		idx := strings.LastIndex(current, "/")
		if idx < 0 {
			break
		}
		current = current[:idx]
	}
	return "", fmt.Errorf("cannot find a mounted ancestor for ZFS dataset %q", dataset)
}

// zfsOneMountpoint returns the mount point for a single dataset without
// walking ancestors. Returns an error when the dataset is unmounted or
// has mountpoint=none/legacy-and-unmounted.
func zfsOneMountpoint(dataset string) (string, error) {
	out, err := exec.Command("zfs", "get", "-H", "-o", "value", "mountpoint", dataset).Output()
	if err != nil {
		return "", err
	}
	mp := strings.TrimSpace(string(out))
	switch mp {
	case "", "none", "-":
		return "", fmt.Errorf("dataset %q has no mountpoint", dataset)
	case "legacy":
		// Mounted via fstab — ask the kernel.
		if fm, err2 := exec.Command("findmnt", "-n", "-o", "TARGET", "--source", dataset).Output(); err2 == nil {
			if t := strings.TrimSpace(string(fm)); t != "" {
				return t, nil
			}
		}
		// Fallback: scan `zfs mount` output.
		if zml, err3 := exec.Command("zfs", "mount").Output(); err3 == nil {
			for _, line := range strings.Split(string(zml), "\n") {
				parts := strings.Fields(line)
				if len(parts) >= 2 && parts[0] == dataset {
					return parts[1], nil
				}
			}
		}
		return "", fmt.Errorf("dataset %q has legacy mountpoint but is not mounted", dataset)
	}
	return mp, nil
}

// isoNameRe validates safe ISO filenames: letters, digits, dots, underscores, hyphens.
// Must end in .iso (case-insensitive).
var isoNameRe = regexp.MustCompile(`^[A-Za-z0-9._\-]+\.iso$`)

// LXDISOInfo describes one ISO file.
type LXDISOInfo struct {
	Name      string    `json:"name"`
	SizeBytes int64     `json:"size_bytes"`
	Modified  time.Time `json:"modified"`
}

// LXDISODir returns the absolute path of the ISO directory for the given Incus
// storage pool.
//
// For ZFS-backed pools the directory is at the ZFS pool mountpoint; for
// dir/btrfs pools it is adjacent to the pool source path. Incus on Debian
// is deb-only (no snap), so QEMU sees the host filesystem directly and the
// ZFS pool's /<mount>/.isos path is reachable without any fd-passing trick.
func LXDISODir(lxdPool string) (string, error) {
	out, err := exec.Command("incus", "query", "/1.0/storage-pools/"+lxdPool).Output()
	if err != nil {
		return "", fmt.Errorf("query LXD pool %q: %w", lxdPool, err)
	}
	var sp struct {
		Driver string            `json:"driver"`
		Config map[string]string `json:"config"`
	}
	if err := json.Unmarshal(out, &sp); err != nil {
		return "", fmt.Errorf("parse pool %q: %w", lxdPool, err)
	}

	var base string
	switch sp.Driver {
	case "zfs":
		zfsPool := sp.Config["zfs.pool_name"]
		if zfsPool == "" {
			zfsPool = sp.Config["source"]
		}
		if zfsPool == "" {
			return "", fmt.Errorf("cannot determine ZFS pool for %q", lxdPool)
		}
		mp, err := zfsResolvedMountpoint(zfsPool)
		if err != nil {
			return "", err
		}
		base = mp
	case "dir", "btrfs", "lvm", "lvm-cluster":
		src := sp.Config["source"]
		if src == "" {
			return "", fmt.Errorf("pool %q has no source path configured", lxdPool)
		}
		if !filepath.IsAbs(src) {
			return "", fmt.Errorf("pool %q source path %q is not absolute", lxdPool, src)
		}
		base = src
	default:
		return "", fmt.Errorf("pool %q has unsupported driver %q for ISO storage", lxdPool, sp.Driver)
	}

	return filepath.Join(base, ".isos"), nil
}

// LXDListISOs returns all .iso files in the pool's .isos directory.
// Returns an empty slice (not an error) when the directory does not exist yet.
func LXDListISOs(lxdPool string) ([]LXDISOInfo, error) {
	dir, err := LXDISODir(lxdPool)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []LXDISOInfo{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read iso dir: %w", err)
	}
	var out []LXDISOInfo
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if !strings.HasSuffix(strings.ToLower(n), ".iso") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, LXDISOInfo{
			Name:      n,
			SizeBytes: info.Size(),
			Modified:  info.ModTime().UTC(),
		})
	}
	if out == nil {
		out = []LXDISOInfo{}
	}
	return out, nil
}

// LXDDeleteISO deletes one ISO file from the pool's .isos directory.
func LXDDeleteISO(lxdPool, filename string) error {
	if !isoNameRe.MatchString(filename) {
		return fmt.Errorf("invalid ISO filename")
	}
	dir, err := LXDISODir(lxdPool)
	if err != nil {
		return err
	}
	return os.Remove(filepath.Join(dir, filename))
}

// LXDSaveISO streams r into <isoDir>/<filename>, creating the directory as needed.
// Returns the absolute path of the saved file.
func LXDSaveISO(lxdPool, filename string, r io.Reader) (string, error) {
	if !isoNameRe.MatchString(filename) {
		return "", fmt.Errorf("invalid ISO filename")
	}
	dir, err := LXDISODir(lxdPool)
	if err != nil {
		return "", err
	}
	// The ISO directory sits inside a ZFS pool root owned by root; use sudo mkdir.
	if out, err2 := exec.Command("sudo", "mkdir", "-p", dir).CombinedOutput(); err2 != nil {
		return "", fmt.Errorf("create iso dir: %s", strings.TrimSpace(string(out)))
	}
	// Ensure the zfsnas process user can write files into the directory.
	if out, err2 := exec.Command("sudo", "chmod", "0775", dir).CombinedOutput(); err2 != nil {
		return "", fmt.Errorf("chmod iso dir: %s", strings.TrimSpace(string(out)))
	}
	dest := filepath.Join(dir, filename)
	// The directory is root-owned; use sudo tee to write the file as root.
	cmd := exec.Command("sudo", "tee", dest)
	cmd.Stdin = r
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		_ = exec.Command("sudo", "rm", "-f", dest).Run()
		return "", fmt.Errorf("write iso: %w", err)
	}
	return dest, nil
}
