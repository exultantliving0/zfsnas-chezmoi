package system

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// binPresenceCache remembers binaries we have already confirmed are installed.
// We only ever cache a *positive* result. This makes "is X installed?" checks
// robust under heavy load: once we have seen a binary, a later transient
// failure (e.g. os.Stat returning EAGAIN/ENOMEM while the box is thrashing, or
// exec.LookPath failing because the process is fork-starved) can never flip the
// answer back to "not installed". A freshly-installed binary is still detected
// because misses are never cached.
var (
	binPresenceMu    sync.RWMutex
	binPresenceCache = map[string]bool{}
)

// commonBinDirs are the standard locations a system binary lives in. We stat
// these directly so detection does not depend on the (often minimal) $PATH the
// zfsnas service is started with — e.g. smbd/exportfs live in /usr/sbin, which
// a stripped systemd PATH may omit.
var commonBinDirs = []string{
	"/usr/local/sbin", "/usr/local/bin",
	"/usr/sbin", "/usr/bin",
	"/sbin", "/bin",
}

// binaryPresent reports whether a single binary is installed, using the sticky
// positive cache plus an explicit scan of commonBinDirs in addition to
// exec.LookPath. Absolute paths are stat'd directly.
func binaryPresent(name string) bool {
	binPresenceMu.RLock()
	cached := binPresenceCache[name]
	binPresenceMu.RUnlock()
	if cached {
		return true
	}

	found := false
	if filepath.IsAbs(name) {
		if _, err := os.Stat(name); err == nil {
			found = true
		}
	} else {
		if _, err := exec.LookPath(name); err == nil {
			found = true
		} else {
			for _, dir := range commonBinDirs {
				if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
					found = true
					break
				}
			}
		}
	}

	if found {
		binPresenceMu.Lock()
		binPresenceCache[name] = true
		binPresenceMu.Unlock()
	}
	return found
}

// ForgetBinaryPresence drops a binary from the sticky positive cache so the
// next presence check re-evaluates from disk. The cache only ever stores
// positives (so transient load failures can't produce a false "not installed"),
// which means an actual *removal* — e.g. uninstalling the virtualization feature
// purges the `incus` binary — would otherwise read as still-installed for the
// life of the process. Call this after a deliberate uninstall so IncusInstalled()
// (and everything keyed on it: the health watchdog, sudoers stripping) reflects
// reality without needing a service restart.
func ForgetBinaryPresence(name string) {
	binPresenceMu.Lock()
	delete(binPresenceCache, name)
	binPresenceMu.Unlock()
}

// binaryInstalled reports whether ALL of the named binaries are installed. Used
// by the various *PrereqsInstalled / Is*Installed helpers so they don't report
// a false "not installed" when the host is too busy to fork/stat reliably.
func binaryInstalled(names ...string) bool {
	for _, n := range names {
		if !binaryPresent(n) {
			return false
		}
	}
	return true
}
