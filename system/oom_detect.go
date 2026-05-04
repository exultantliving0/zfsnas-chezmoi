package system

import (
	"bufio"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OOMEvent records one kernel OOM-kill incident scraped from journalctl.
type OOMEvent struct {
	Time   time.Time `json:"time"`
	PID    int       `json:"pid"`
	Comm   string    `json:"comm"`   // killed process basename, e.g. "qemu-system-x86"
	Cgroup string    `json:"cgroup"` // task_memcg path when present, e.g. "/lxc.payload.win-vm-1"
	Raw    string    `json:"raw"`    // full kernel log line
}

// ScanRecentOOMEvents returns OOM-kill events from the kernel ring buffer
// within `window` (e.g. 5 * time.Minute). Results are cached for 15 s to
// avoid hammering journalctl when several state-watcher ticks land at once.
//
// We try `journalctl` directly first; on hardened sudoers hosts the user
// isn't in the systemd-journal/adm group so we fall back to sudo. Both
// failure modes return an empty slice — OOM annotation is best-effort.
func ScanRecentOOMEvents(window time.Duration) []OOMEvent {
	oomCacheMu.Lock()
	defer oomCacheMu.Unlock()
	if !oomCacheAt.IsZero() && time.Since(oomCacheAt) < 15*time.Second {
		return filterOOMEventsAfter(oomCacheData, time.Now().Add(-window))
	}
	events := readOOMFromJournal(window)
	oomCacheAt = time.Now()
	oomCacheData = events
	return filterOOMEventsAfter(events, time.Now().Add(-window))
}

var (
	oomCacheMu   sync.Mutex
	oomCacheAt   time.Time
	oomCacheData []OOMEvent
)

func filterOOMEventsAfter(events []OOMEvent, threshold time.Time) []OOMEvent {
	out := make([]OOMEvent, 0, len(events))
	for _, e := range events {
		if e.Time.After(threshold) {
			out = append(out, e)
		}
	}
	return out
}

func readOOMFromJournal(window time.Duration) []OOMEvent {
	since := "-" + strconv.FormatInt(int64(window.Seconds()), 10) + "s"
	args := []string{"--no-pager", "-q", "--since=" + since, "-o", "short-iso"}

	out, err := exec.Command("journalctl", args...).Output()
	if err != nil {
		// Fall back to sudo (most common path on hosts where the service
		// account isn't in systemd-journal / adm).
		fullArgs := append([]string{"-n", "/usr/bin/journalctl"}, args...)
		out, err = exec.Command("sudo", fullArgs...).Output()
		if err != nil {
			return nil
		}
	}
	return parseOOMLines(string(out))
}

// Regexes that catch the meaningful pieces of a kernel OOM event. The kernel
// emits several lines per kill; we synthesise one OOMEvent per matching line
// and let the consumer dedupe on (time, pid).
var (
	// "Killed process 12345 (qemu-system-x86) total-vm:..."
	reKilledProc = regexp.MustCompile(`Killed process (\d+) \(([^)]+)\)`)
	// "oom_reaper: reaped process 12345 (qemu-system-x86), now anon-rss:..."
	reReaped = regexp.MustCompile(`oom_reaper: reaped process (\d+) \(([^)]+)\)`)
	// "oom-kill:constraint=..., task=qemu-system-x86,pid=12345,..."
	reOomKill = regexp.MustCompile(`oom-kill:.*task=([^,]+),pid=(\d+)`)
	// task_memcg=/lxc.payload.win-vm-1
	reTaskMemcg = regexp.MustCompile(`task_memcg=(\S+)`)
	// systemd line: "<unit>.service: A process of this unit has been killed by the OOM killer."
	reSystemdOOM = regexp.MustCompile(`(\S+\.(?:service|scope)): A process of this unit has been killed by the OOM killer`)
	// Leading ISO timestamp from `journalctl -o short-iso` output, e.g.
	// "2026-05-04T10:11:01-0400 host kernel: ..."
	reISOTimestamp = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}[+-]\d{4})`)
)

func parseOOMLines(out string) []OOMEvent {
	var events []OOMEvent
	scanner := bufio.NewScanner(strings.NewReader(out))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	var pendingCgroup string
	for scanner.Scan() {
		line := scanner.Text()
		ts := parseJournalISOTime(line)

		// Capture the task_memcg from any preceding line so it can attach
		// to the next "Killed process" entry. The kernel emits the dump
		// table several lines before the kill verdict.
		if m := reTaskMemcg.FindStringSubmatch(line); len(m) >= 2 {
			pendingCgroup = m[1]
		}

		var ev *OOMEvent
		switch {
		case reKilledProc.MatchString(line):
			m := reKilledProc.FindStringSubmatch(line)
			pid, _ := strconv.Atoi(m[1])
			ev = &OOMEvent{Time: ts, PID: pid, Comm: m[2], Cgroup: pendingCgroup, Raw: line}
		case reReaped.MatchString(line):
			m := reReaped.FindStringSubmatch(line)
			pid, _ := strconv.Atoi(m[1])
			ev = &OOMEvent{Time: ts, PID: pid, Comm: m[2], Cgroup: pendingCgroup, Raw: line}
		case reOomKill.MatchString(line):
			m := reOomKill.FindStringSubmatch(line)
			pid, _ := strconv.Atoi(m[2])
			ev = &OOMEvent{Time: ts, PID: pid, Comm: m[1], Cgroup: pendingCgroup, Raw: line}
		case reSystemdOOM.MatchString(line):
			m := reSystemdOOM.FindStringSubmatch(line)
			ev = &OOMEvent{Time: ts, Comm: m[1], Raw: line}
		}
		if ev != nil {
			events = append(events, *ev)
			pendingCgroup = "" // consume so it doesn't bleed into the next event
		}
	}
	return events
}

func parseJournalISOTime(line string) time.Time {
	m := reISOTimestamp.FindString(line)
	if m == "" {
		return time.Now()
	}
	t, err := time.Parse("2006-01-02T15:04:05-0700", m)
	if err != nil {
		return time.Now()
	}
	return t
}

// LooksLikeOOMKilled returns true if any OOM-kill event in the recent journal
// can plausibly be tied to the given Incus instance. Matching strategy:
//  1. Cgroup path contains "lxc.payload.<name>" / "lxc.monitor.<name>" — strong match.
//  2. Otherwise, any OOM event mentioning a "qemu-system" or "lxc-start" comm
//     within `window` is treated as a temporal match for VMs/containers — the
//     state watcher only calls this for instances that actually transitioned
//     to Stopped/Crashed, so false positives are rare in practice and the
//     activity-log entry remains worth surfacing.
func LooksLikeOOMKilled(name string, window time.Duration) bool {
	events := ScanRecentOOMEvents(window)
	if len(events) == 0 {
		return false
	}
	cgroupHints := []string{
		"lxc.payload." + name,
		"lxc.monitor." + name,
		"/" + name,
	}
	temporal := false
	for _, e := range events {
		if e.Cgroup != "" {
			for _, hint := range cgroupHints {
				if strings.Contains(e.Cgroup, hint) {
					return true
				}
			}
		}
		// systemd unit lines (incus.service / lxc.payload-<name>.scope)
		// also embed the instance name when the kernel cgroup is a scope.
		if e.Comm != "" {
			for _, hint := range cgroupHints {
				if strings.Contains(e.Comm, hint) {
					return true
				}
			}
		}
		if e.Comm == "qemu-system-x86" || e.Comm == "qemu-system-x86_64" ||
			e.Comm == "lxc-start" || e.Comm == "lxc-monitor" ||
			strings.Contains(e.Comm, "incus.service") {
			temporal = true
		}
	}
	return temporal
}
