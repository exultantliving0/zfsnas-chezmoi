package system

import (
	"log"
	"strings"
	"sync"
	"time"

	"zfsnas/internal/audit"
)

// StartLXDStateWatcher starts a background goroutine that polls Incus instance
// statuses every 10 seconds and emits an audit-log entry whenever a VM or
// container changes state — including state changes that happen outside the
// portal (CLI, host reboot, autostart on boot, qemu crash, …).
//
// Behaviour:
//   - First successful poll establishes a baseline; nothing is logged.
//   - On each subsequent poll, instances that flipped state vs. the previous
//     snapshot generate one audit entry with action ActionIncusStateChange,
//     user "system", and details "<old> → <new>".
//   - Brand-new instances (e.g. just created) are added to the baseline
//     silently; the dedicated incus_create_* audit covers their birth event.
//   - Instances that disappeared from the list are dropped from the map; the
//     dedicated incus_delete audit covers that event.
//
// Hot-loops are avoided: when LXDAvailable() returns false we sleep one tick
// without listing. This lets the watcher run unconditionally — it costs
// nothing on hosts that haven't enabled the VMs & Containers feature.
func StartLXDStateWatcher() {
	go lxdStateWatcherLoop()
}

var (
	lxdStateOnce sync.Once
	lxdStateMu   sync.Mutex
	lxdStateMap  = map[string]string{}
	lxdStatePrimed bool
)

func lxdStateWatcherLoop() {
	// Make double-start a no-op (defensive against accidental wiring).
	started := false
	lxdStateOnce.Do(func() { started = true })
	if !started {
		return
	}
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for range t.C {
		lxdStateWatcherTick()
	}
}

func lxdStateWatcherTick() {
	if !LXDAvailable() {
		return
	}
	insts, err := ListLXDInstances()
	if err != nil {
		// Transient errors (Incus restarting, race with a config edit) are
		// silent; persistent failure shows up via LXDAvailable() returning
		// false on the next probe.
		return
	}

	lxdStateMu.Lock()
	defer lxdStateMu.Unlock()

	// Build the current snapshot first so a partial list doesn't poison the
	// baseline.
	now := map[string]string{}
	for _, inst := range insts {
		now[inst.Name] = canonicalLXDStatus(inst.Status)
	}

	if !lxdStatePrimed {
		lxdStateMap = now
		lxdStatePrimed = true
		return
	}

	// Diff: emit one audit entry per instance whose state has changed.
	for name, newSt := range now {
		oldSt, known := lxdStateMap[name]
		if !known {
			// New instance — silently absorb. The create handler already
			// emitted incus_create_*.
			continue
		}
		if oldSt == newSt {
			continue
		}
		details := oldSt + " → " + newSt
		// When a VM/container left the Running state, look for a recent
		// kernel OOM event tied to this instance. If we find one, surface
		// it in the activity feed: it's a much more useful signal than a
		// bare "Running → Stopped" when the host ran out of RAM. The OOM
		// scan is best-effort — fails silently when journalctl isn't
		// reachable.
		if oldSt == "Running" && (newSt == "Stopped" || newSt == "Crashed" || newSt == "Error" || newSt == "Aborted") {
			if LooksLikeOOMKilled(name, 90*time.Second) {
				details += " (system out of memory)"
			}
		}
		audit.Log(audit.Entry{
			User:    "system",
			Role:    "system",
			Action:  audit.ActionIncusStateChange,
			Target:  name,
			Result:  audit.ResultOK,
			Details: details,
		})
		log.Printf("LXD state watcher: %s %s", name, details)
	}
	// Drop deleted instances; the delete handler already emitted incus_delete.
	lxdStateMap = now
}

// canonicalLXDStatus normalises Incus' status strings (which can include
// trailing whitespace or odd casing on rare occasions) so the diff is stable.
// Empty input becomes "Unknown" so a transient blank doesn't churn the log.
func canonicalLXDStatus(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "Unknown"
	}
	return s
}
