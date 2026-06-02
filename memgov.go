package main

import (
	"log"
	"os"
	"runtime"
	"runtime/debug"
	"time"
)

// startMemoryGovernor tames the process RSS without dropping any feature.
//
// Why this is needed: at startup we Open() every RRD database (capacity +
// per-LXD-instance metrics), which parses on the order of a couple hundred MB
// of JSON. The Go runtime keeps that freed memory as reusable heap and only
// returns it to the OS lazily, so the RSS stays inflated (~1 GB observed) long
// after the parse is done. There is nothing to "fix" in the data — most of it
// is reclaimable slack.
//
// Two levers, both safe and override-able by the operator via env:
//   - GOGC lowered to 50 (unless GOGC is set): the GC runs when the heap has
//     grown 50% past the live set instead of 100%, so steady-state slack is
//     roughly halved. Costs a little more CPU on a service that is mostly idle.
//   - A periodic debug.FreeOSMemory(): forces a GC and hands idle heap pages
//     back to the OS. This is what reclaims the big one-shot startup-parse
//     slack. Unlike a too-low GOMEMLIMIT it cannot cause a GC death-spiral —
//     it runs on a timer, not reactively against a hard ceiling.
//
// Each tick also logs live-heap vs OS-released numbers so the actual working
// set is observable (HeapInuse is the real floor; Sys-Released is the slack we
// just returned).
func startMemoryGovernor() {
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(50)
	}

	// Return startup-parse slack promptly, then keep a slow background sweep.
	go func() {
		// First sweep shortly after boot once all collectors have Open()ed
		// their RRD files and the transient parse garbage is collectable.
		time.Sleep(90 * time.Second)
		freeAndLog("post-startup")

		t := time.NewTicker(15 * time.Minute)
		defer t.Stop()
		for range t.C {
			freeAndLog("periodic")
		}
	}()
}

func freeAndLog(reason string) {
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	debug.FreeOSMemory()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	const mb = 1024 * 1024
	log.Printf("[memgov] %s: heap_inuse=%dMB heap_idle=%dMB released_to_os=%dMB sys=%dMB (freed %dMB this sweep)",
		reason,
		after.HeapInuse/mb,
		after.HeapIdle/mb,
		after.HeapReleased/mb,
		after.Sys/mb,
		(int64(after.HeapReleased)-int64(before.HeapReleased))/mb,
	)
}
