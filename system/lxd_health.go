package system

// Incus daemon health watchdog (v6.5.26 fix).
//
// Reason this exists: every ZNAS handler that shells out to `incus …`
// inherits the daemon's responsiveness. When the daemon hangs (cluster
// recovery, broken socket, leaked lock, kernel-namespace stall…), each
// hit spawns a goroutine that blocks forever and the portal looks
// frozen even though its own listener is up. Worse, before the 6.5.26
// startup-goroutine fix, the very first `incus list` at boot would
// wedge main before the HTTPS listener bound — service appeared running
// with sudo activity in the logs, but :8443 never came up.
//
// The watchdog runs in its own goroutine, calls `incus list` with a
// bounded deadline, and toggles a `stuck=true/false` state. State
// transitions broadcast to every browser session on /ws/alerts so the
// UI can surface a persistent red banner (auto-clears when the daemon
// recovers). Two consecutive failures before flipping to `stuck` keeps
// a single transient `incus list` slow tick from spamming the banner.

import (
	"sync"
	"time"
)

// IncusHealthState is what the watchdog broadcasts on state change and
// what /api/incus/health serves to brand-new browser sessions that
// reconnect after a flap.
type IncusHealthState struct {
	Stuck     bool      `json:"stuck"`
	LastError string    `json:"last_error,omitempty"`
	ChangedAt time.Time `json:"changed_at"`
}

var (
	incusHealthMu      sync.RWMutex
	incusHealthCurrent = IncusHealthState{Stuck: false, ChangedAt: time.Now()}
)

// IncusHealth returns the latest probed state. Safe to call from any
// goroutine; reads are RLock-cheap.
func IncusHealth() IncusHealthState {
	incusHealthMu.RLock()
	defer incusHealthMu.RUnlock()
	return incusHealthCurrent
}

// StartIncusHealthWatcher launches a background poller that runs
// `incus list` with a 10 s deadline every 30 s. The callback fires
// only on state transitions (healthy→stuck, stuck→healthy), so the
// alerts hub doesn't see chatter on every successful probe. Caller is
// responsible for delivering the state change to whatever sink they
// want (WebSocket hub, log, audit, …).
//
// A single transient failure does NOT flip the state — we require two
// consecutive failures before declaring "stuck" so a one-off slow tick
// doesn't paint the banner. Recovery is immediate (one success clears).
func StartIncusHealthWatcher(onChange func(IncusHealthState)) {
	const (
		interval        = 30 * time.Second
		probeTimeout    = 10 * time.Second
		stuckThreshold  = 2 // consecutive failures before flipping to stuck
	)
	go func() {
		fails := 0
		var lastErr string
		for {
			if LXDAvailableTimeout(probeTimeout) {
				lastErr = ""
				fails = 0
				// Was stuck → recovered. Broadcast once.
				incusHealthMu.Lock()
				wasStuck := incusHealthCurrent.Stuck
				if wasStuck {
					incusHealthCurrent = IncusHealthState{Stuck: false, ChangedAt: time.Now()}
				}
				snap := incusHealthCurrent
				incusHealthMu.Unlock()
				if wasStuck && onChange != nil {
					onChange(snap)
				}
			} else {
				fails++
				lastErr = "incus list timed out or failed"
				if fails >= stuckThreshold {
					incusHealthMu.Lock()
					wasOK := !incusHealthCurrent.Stuck
					if wasOK {
						incusHealthCurrent = IncusHealthState{
							Stuck:     true,
							LastError: lastErr,
							ChangedAt: time.Now(),
						}
					}
					snap := incusHealthCurrent
					incusHealthMu.Unlock()
					if wasOK && onChange != nil {
						onChange(snap)
					}
				}
			}
			time.Sleep(interval)
		}
	}()
}
