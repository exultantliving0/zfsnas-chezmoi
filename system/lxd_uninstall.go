package system

// Uninstall counterpart of LXDEnableFeature. Removes Incus and the QEMU
// virtualisation stack but deliberately keeps the network bridges in
// /etc/network/interfaces and the chrony time-sync setup, so re-enabling
// later is fast and the host's connectivity isn't disturbed.
//
// Pre-flight: must have zero VMs / zero containers. The HTTP layer
// enforces this; the function itself bails if it sees any.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// LXDInstanceCounts is the result of a count probe used by the uninstall
// pre-flight. The frontend renders these next to the Uninstall button so
// the user sees exactly why the operation is or isn't allowed.
type LXDInstanceCounts struct {
	VMCount        int  `json:"vm_count"`
	ContainerCount int  `json:"container_count"`
	CanUninstall   bool `json:"can_uninstall"`
}

// LXDCountInstances returns the number of VMs and containers Incus knows
// about. When the daemon isn't reachable the counts come back as zero —
// uninstall is fine in that case (nothing to lose).
func LXDCountInstances() LXDInstanceCounts {
	out := LXDInstanceCounts{}
	// `incus list --format csv -c t` gives one TYPE token per instance.
	// Reliable across Incus versions; doesn't depend on the pretty-print
	// shape of /1.0/instances?recursion=1 (which has variable whitespace
	// between key and value).
	raw, err := exec.Command("incus", "list", "--format", "csv", "-c", "t").Output()
	if err != nil {
		// Daemon not running / not installed — nothing to count, uninstall safe.
		out.CanUninstall = true
		return out
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		switch strings.TrimSpace(strings.ToUpper(line)) {
		case "VIRTUAL-MACHINE":
			out.VMCount++
		case "CONTAINER":
			out.ContainerCount++
		}
	}
	out.CanUninstall = (out.VMCount + out.ContainerCount) == 0
	return out
}

// lxdUninstallPackages is the apt purge list. Mirrors the install list in
// system/lxd_enable.go (`incus*`, `qemu-system-x86`, `bridge-utils`,
// `dnsmasq-base`, `swtpm`, `ovmf`, `sshpass`, `genisoimage`). chrony is
// intentionally omitted — many users want it for time sync regardless of
// virtualisation.
var lxdUninstallPackages = []string{
	"incus", "incus-base", "incus-client", "incus-extra", "incus-agent",
	"qemu-system-x86", "swtpm", "ovmf", "sshpass", "genisoimage",
	"bridge-utils", "dnsmasq-base",
}

// LXDUninstallFeature stops the Incus services, purges the install set and
// removes Incus' on-disk state. The network bridges in
// /etc/network/interfaces and chrony are left exactly as the enable flow
// configured them so the user's connectivity / time sync continue to work.
func LXDUninstallFeature(ctx context.Context, job *LXDEnableJob) {
	// Re-define the steps for the uninstall job. The job struct is shared
	// with the enable flow but the step list is an array we can swap.
	job.mu.Lock()
	job.Steps = []LXDEnableStepStatus{
		{ID: 1, Label: "Verify no instances are deployed", Status: "pending"},
		{ID: 2, Label: "Stop Incus daemon", Status: "pending"},
		{ID: 3, Label: "Purge Incus and QEMU packages", Status: "pending"},
		{ID: 4, Label: "Remove /var/lib/incus and /etc/incus", Status: "pending"},
		{ID: 5, Label: "Re-assert ifupdown over netplan", Status: "pending"},
	}
	job.mu.Unlock()

	finish := func(err error) {
		job.mu.Lock()
		if err != nil {
			job.Status = "error"
			job.Error = err.Error()
		} else {
			job.Status = "done"
		}
		job.mu.Unlock()
	}

	// Step 1 — pre-flight count guard. Even though the HTTP layer checks
	// this, race conditions (someone creates a VM between the check and
	// the start) are possible; bail safely.
	job.setStep(1, "running", "")
	counts := LXDCountInstances()
	if !counts.CanUninstall {
		msg := fmt.Sprintf("uninstall blocked: %d VM(s) and %d container(s) still deployed",
			counts.VMCount, counts.ContainerCount)
		job.setStep(1, "error", msg)
		finish(fmt.Errorf("%s", msg))
		return
	}
	job.setStep(1, "done", "")
	job.log(fmt.Sprintf("Pre-flight passed: %d VMs / %d containers — proceeding.",
		counts.VMCount, counts.ContainerCount))

	// Step 2 — stop the Incus daemon and its activation socket. Both
	// commands tolerate "not loaded" so an already-removed install
	// finishes cleanly.
	job.setStep(2, "running", "")
	for _, unit := range []string{"incus.service", "incus.socket"} {
		_ = runCmdLog(ctx, job, "/usr/bin/systemctl", "stop", unit)
		_ = runCmdLog(ctx, job, "/usr/bin/systemctl", "disable", unit)
	}
	job.setStep(2, "done", "")

	// Step 3 — apt-get purge for the packages WE explicitly installed.
	// We deliberately do NOT run `apt-get autoremove` — it would sweep
	// `ifupdown` out as a no-longer-needed dependency, which dpkg-purges
	// /etc/network/interfaces with it (`ifupdown` owns that conffile).
	// The user wants the network bridges preserved, so we leave the
	// dependency closure in place. The user can run `apt autoremove`
	// manually later if they want to reclaim disk.
	job.setStep(3, "running", "")
	args := append([]string{"/usr/bin/apt-get", "purge", "-y"}, lxdUninstallPackages...)
	job.log("Running apt-get purge…")
	if err := runCmdLog(ctx, job, args[0], args[1:]...); err != nil {
		// apt-get purge returns non-zero if a package isn't installed; we
		// don't want that to abort. Log and continue.
		job.log("apt-get purge non-zero (ignored — packages may already be absent): " + err.Error())
	}
	job.setStep(3, "done", "")

	// Step 4 — remove daemon state. Incus stores DB + per-instance config
	// under /var/lib/incus and per-host overrides under /etc/incus; both
	// are useless after the daemon is gone.
	job.setStep(4, "running", "")
	for _, p := range []string{"/var/lib/incus", "/etc/incus"} {
		if _, err := os.Stat(p); err == nil {
			job.log("Removing " + p + "…")
			if err := runCmdLog(ctx, job, "/usr/bin/rm", "-rf", p); err != nil {
				job.log("Warning: rm -rf " + p + ": " + err.Error())
			}
		}
	}
	job.setStep(4, "done", "")

	// Step 5 — re-assert the post-migration network state.
	//
	// Why this is necessary: cloud-init (or some package's postinst on
	// the next boot) can drop a fresh /etc/netplan/<name>.yaml back into
	// place even after the migration renamed it to <name>.yaml.znas-disabled.
	// If that happens AND the user later re-enables the feature, the
	// prereq sees both ifupdown and an active netplan YAML and gets
	// confused. Belt-and-braces: every uninstall (a) removes any *.yaml
	// in /etc/netplan/ that has a .znas-disabled sibling — those are
	// migration-disabled and shouldn't be active; (b) re-drops the
	// cloud-init "network: {config: disabled}" file so a future boot
	// doesn't regenerate them.
	job.setStep(5, "running", "")
	if err := lxdReassertIfupdownOverNetplan(job); err != nil {
		// Non-fatal — uninstall has already succeeded at the package
		// level and the user can clean these up manually.
		job.log("Warning: " + err.Error())
	}
	job.setStep(5, "done", "")

	job.log("Done. Network bridges in /etc/network/interfaces and chrony left intact.")
	finish(nil)
}

// lxdReassertIfupdownOverNetplan removes any .yaml in /etc/netplan/ whose
// migration-disabled sibling (.yaml.znas-disabled) exists, and re-drops
// the cloud-init network-disable file so a later boot doesn't regenerate
// netplan YAMLs. No-op when /etc/network/interfaces is missing (no
// migration happened, leave the system alone).
func lxdReassertIfupdownOverNetplan(job *LXDEnableJob) error {
	if _, err := os.Stat("/etc/network/interfaces"); err != nil {
		// No ifupdown stanza file → user never migrated, hands-off.
		return nil
	}

	// (a) Remove any /etc/netplan/<name>.yaml that has a sibling
	// <name>.yaml.znas-disabled. Those are exactly the files the migration
	// neutralised; if they're back, dpkg or cloud-init re-deployed them.
	disabled, _ := filepath.Glob("/etc/netplan/*.yaml.znas-disabled")
	for _, d := range disabled {
		live := strings.TrimSuffix(d, ".znas-disabled")
		if _, err := os.Stat(live); err != nil {
			continue
		}
		job.log("Removing recreated " + live + " (migration disabled it as " + d + ").")
		if out, err := exec.Command("sudo", "/usr/bin/rm", "-f", live).CombinedOutput(); err != nil {
			job.log("  ⚠ rm failed: " + strings.TrimSpace(string(out)))
		}
	}

	// (b) Drop the cloud-init disable file if cloud-init exists. Same
	// content the migration writes — duplicating it here covers the case
	// where the migration ran on a host that didn't have cloud-init at
	// the time but does now (e.g. apt installed something else that
	// pulled cloud-init in).
	if _, err := os.Stat("/etc/cloud/cloud.cfg.d"); err == nil {
		const path = "/etc/cloud/cloud.cfg.d/99-znas-disable-network-config.cfg"
		body := "# Written by ZNAS so cloud-init does not regenerate /etc/netplan/*.yaml\n" +
			"# after the netplan→ifupdown migration. Removing this file lets cloud-init\n" +
			"# manage the network again.\nnetwork: {config: disabled}\n"
		// Only write if missing — never overwrite a user-edited file.
		if _, err := os.Stat(path); err != nil {
			job.log("Re-asserting cloud-init disable → " + path)
			if err := writeRoot(path, []byte(body), 0o644); err != nil {
				job.log("  ⚠ could not write " + path + ": " + err.Error())
			}
		}
	}

	return nil
}
