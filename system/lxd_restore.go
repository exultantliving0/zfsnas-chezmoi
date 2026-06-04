package system

// Restore helpers for the VM/Container backup feature (v6.5.19+).
//
// Two paths:
//   • Instant restore — rename in place. The bkup--<vm> Incus instance is
//     renamed to a user-chosen name and becomes a regular VM/Container.
//     No data is copied. Only valid when the backup lives on THIS host.
//   • Clone restore — syncoid copy of the backup dataset (local or remote)
//     into a chosen local Incus storage pool, then `incus admin recover` so
//     the new dataset shows up as a registered Incus instance.

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// LXDInstantRestoreBackup turns a local backup into a regular Incus
// instance under <newName>, in place — no data is copied.
//
// v6.5.19+: backups live as plain ZFS datasets at
// "<zfs-pool>/ZNAS-Backups-Workload/<kind>/bkup--<vm>", NOT as registered
// Incus instances. So the operation is:
//
//  1. Locate the workload dataset on the named ZFS pool. zfsPool may be
//     "" in which case all imported pools are scanned for a match.
//  2. Find the Incus storage pool that uses this ZFS pool as source (the
//     "incus_datastore" surfaced by the picker). If none exists, the
//     restore is rejected — the dataset has nowhere to land that Incus
//     would discover.
//  3. zfs-rename the dataset (and its .block sibling) out of the workload
//     parent and INTO "<incus-pool-source>/<kind>/<newName>".
//  4. Rewrite backup.yaml (pool, instance name, clear snapshots).
//  5. Destroy snapshots on the renamed dataset so its count matches the
//     freshly cleared backup.yaml.
//  6. Run `incus admin recover` to register it.
//  7. Reset volatile state so the clone gets fresh MACs etc.
//
// The legacy code path (Incus-registered bkup--<vm>) is still attempted
// first when no workload dataset is found, so older test data still
// responds.
func LXDInstantRestoreBackup(vmID, newName, zfsPool string) error {
	if vmID == "" || newName == "" {
		return fmt.Errorf("vm_id and new_name are required")
	}
	if !lxdNameRe.MatchString(newName) {
		return fmt.Errorf("invalid new name %q (Incus naming rules)", newName)
	}
	if IsBackupInstanceName(newName) {
		return fmt.Errorf("new name cannot start with %q", LXDBackupPrefix)
	}
	src := LXDBackupPrefix + vmID
	if _, err := LXDGetStatus(newName); err == nil {
		return fmt.Errorf("instance %q already exists", newName)
	}

	// --- Workload layout (v6.5.19+ canonical) -----------------------------
	if pool, kind := findWorkloadBackup(src, zfsPool); pool != "" {
		return instantRestoreFromWorkload(pool, kind, vmID, newName)
	}

	// --- Legacy Incus-registered fallback ---------------------------------
	if status, err := LXDGetStatus(src); err == nil {
		if !strings.EqualFold(status, "Stopped") {
			return fmt.Errorf("backup instance %s must be Stopped (current: %s)", src, status)
		}
		if out, err := exec.Command("incus", "rename", src, newName).CombinedOutput(); err != nil {
			return fmt.Errorf("incus rename: %s", strings.TrimSpace(string(out)))
		}
		resetCloneVolatileState(newName, nil)
		return nil
	}

	return fmt.Errorf("backup %s not found on this host (workload nor Incus layout)", src)
}

// findWorkloadBackup locates the workload-layout backup for `bkup--<vm>`.
// When zfsPool is empty, it scans every imported pool; otherwise only that
// pool is checked. Returns the pool and the kind ("virtual-machines" or
// "containers") when found; ("","") otherwise.
func findWorkloadBackup(backupName, zfsPool string) (string, string) {
	pools, _ := ListLocalZFSPools()
	if zfsPool != "" {
		pools = []string{zfsPool}
	}
	for _, pool := range pools {
		parent := LXDWorkloadBackupParent(pool)
		for _, kind := range []string{"virtual-machines", "containers"} {
			if datasetExists(parent + "/" + kind + "/" + backupName) {
				return pool, kind
			}
		}
	}
	return "", ""
}

// instantRestoreFromWorkload moves a workload-layout backup into the
// Incus virtual-machines/containers/ subtree, rewrites backup.yaml, and
// runs incus admin recover.
func instantRestoreFromWorkload(zfsPool, kind, vmID, newName string) error {
	// Find which Incus storage pool sources from this ZFS pool. Without
	// an Incus pool, recover can't register the dataset.
	incusPool, incusSource := findIncusPoolForZFSPool(zfsPool)
	if incusPool == "" {
		return fmt.Errorf("ZFS pool %q has no Incus storage pool configured — clone-restore into another datastore instead", zfsPool)
	}

	bkup := LXDBackupPrefix + vmID
	parent := LXDWorkloadBackupParent(zfsPool)
	srcRoot := parent + "/" + kind + "/" + bkup
	srcBlock := srcRoot + ".block"
	hasBlock := kind == "virtual-machines" && datasetExists(srcBlock)

	dstParent := incusSource + "/" + kind
	dstRoot := dstParent + "/" + newName
	dstBlock := dstRoot + ".block"

	// Ensure parent path exists on the Incus side.
	_ = exec.Command("sudo", "zfs", "create", "-p", "-o", "mountpoint=none", dstParent).Run()

	if out, err := exec.Command("sudo", "zfs", "rename", srcRoot, dstRoot).CombinedOutput(); err != nil {
		return fmt.Errorf("zfs rename root-fs: %s", strings.TrimSpace(string(out)))
	}
	if hasBlock {
		if out, err := exec.Command("sudo", "zfs", "rename", srcBlock, dstBlock).CombinedOutput(); err != nil {
			// Rollback root rename — partial state is bad.
			_, _ = exec.Command("sudo", "zfs", "rename", dstRoot, srcRoot).CombinedOutput()
			return fmt.Errorf("zfs rename .block: %s", strings.TrimSpace(string(out)))
		}
	}

	// Destroy snapshots on the renamed datasets so the cleared
	// snapshots: [] list in backup.yaml lines up with on-disk state.
	destroyDatasetSnapshots(dstRoot, nil)
	if hasBlock {
		destroyDatasetSnapshots(dstBlock, nil)
	}

	// Rewrite backup.yaml inside the renamed root-fs dataset. This path only
	// restores the root (+.block), so any attached custom-volume disk is not
	// present — pass an empty captured-set to strip those devices.
	if err := LXDRewriteBackupYAMLForRestore(dstRoot, incusPool, incusSource, vmID, newName, map[string]string{}); err != nil {
		// non-fatal — recover will still attempt
	}

	// Run incus admin recover, masking other backup datasets so recover
	// only registers this one. If recover fails, move the dataset back
	// to its workload home so the user can retry without losing the
	// backup or stranding a dataset that Incus doesn't know about.
	out, err := LXDIncusAdminRecoverWithMask("")
	if err != nil {
		_, _ = exec.Command("sudo", "zfs", "rename", dstRoot, srcRoot).CombinedOutput()
		if hasBlock {
			_, _ = exec.Command("sudo", "zfs", "rename", dstBlock, srcBlock).CombinedOutput()
		}
		return fmt.Errorf("incus admin recover: %s", strings.TrimSpace(string(out)))
	}
	// Clear volatile state (MAC, vsock id, …) so the new instance gets
	// fresh values instead of colliding with the original VM.
	resetCloneVolatileState(newName, nil)
	return nil
}

// findIncusPoolForZFSPool returns the (Incus pool name, its zfs `source`)
// for the Incus storage pool that uses `zfsPool` as its top-level pool.
// Returns ("","") when none is configured (or Incus isn't installed).
func findIncusPoolForZFSPool(zfsPool string) (string, string) {
	pools, _ := LXDListStoragePools()
	want := strings.ToLower(zfsPool)
	for _, p := range pools {
		src := LXDStoragePoolSource(p)
		if src == "" {
			continue
		}
		root := src
		if i := strings.IndexByte(src, '/'); i > 0 {
			root = src[:i]
		}
		if strings.ToLower(root) == want {
			return p, src
		}
	}
	return "", ""
}

// LXDCloneRestoreLocal performs a clone-restore from a local bkup--<vmID>
// dataset on this host into a chosen local Incus storage pool, registering
// the resulting dataset as a fresh Incus instance named <cloneName>.
//
// When `snapshotName` is non-empty, the cloned destination dataset is
// rolled back to that exact snapshot before `incus admin recover` runs —
// so the resulting instance reflects the VM state at that point in time
// instead of the latest. Empty `snapshotName` keeps the latest state.
//
// Steps:
//  1. resolve source dataset (this host)
//  2. resolve destination dataset (local Incus pool with kind matching)
//  3. syncoid local replication
//  4. zfs rename the received bkup--<vmID> dataset to <cloneName>
//  5. zfs rollback -r <dst>@<snapshotName>  (if snapshotName != "")
//  6. incus admin recover on the destination pool so it surfaces
func LXDCloneRestoreLocal(ctx context.Context, vmID, srcDatastore, dstDatastore, cloneName, snapshotName string, logFn func(string)) error {
	if !lxdNameRe.MatchString(cloneName) {
		return fmt.Errorf("invalid clone name %q", cloneName)
	}
	if IsBackupInstanceName(cloneName) {
		return fmt.Errorf("clone name cannot start with %q", LXDBackupPrefix)
	}
	if _, err := LXDGetStatus(cloneName); err == nil {
		return fmt.Errorf("instance %q already exists", cloneName)
	}

	dstSource := getLXDPoolSource(dstDatastore)
	if dstSource == "" {
		return fmt.Errorf("destination datastore %q has no zfs source", dstDatastore)
	}

	backupName := LXDBackupPrefix + vmID

	// v6.5.19: backups are not registered with Incus, so we enumerate
	// their constituent datasets via ZFS scan instead of `incus list`.
	// This also returns the detected `kind` ("virtual-machines" or
	// "containers") since we can't query Incus for that either.
	parts, kind, err := LXDBackupInstanceDatasets(backupName, srcDatastore)
	if err != nil {
		return fmt.Errorf("locate backup instance: %w", err)
	}

	// Map each captured custom volume's original name → the name it's restored
	// under. Same on recovery (cloneName==vmID); on clone-to-new-name we make
	// it unique so it can't collide with the still-running source's volume.
	// Attached disk devices whose volume ISN'T captured get stripped.
	volRemap := map[string]string{}
	for _, pt := range parts {
		if pt.Kind != "custom" {
			continue
		}
		origVol := strings.TrimPrefix(pt.DstBaseName, backupName+".")
		newVol := origVol
		if cloneName != vmID {
			if strings.Contains(origVol, vmID) {
				newVol = strings.Replace(origVol, vmID, cloneName, 1)
			} else {
				newVol = cloneName + "-" + origVol
			}
		}
		volRemap[origVol] = newVol
	}

	// Ensure parent destination datasets exist.
	dstParent := dstSource + "/" + kind
	_ = exec.Command("sudo", "zfs", "create", "-p", dstParent).Run()
	_ = exec.Command("sudo", "zfs", "create", "-p", dstSource+"/custom").Run()

	// For each part, syncoid into a temporary landing dataset, then rename
	// to the user-chosen name. The naming substitutes bkup--<vmID> in the
	// part's DstBaseName with the user's cloneName so the renamed instance
	// is internally consistent (e.g. ".block" zvol carries the new name).
	for _, part := range parts {
		// Replace the "bkup--<vmID>" prefix in the part's basename so the
		// landing dataset already carries the clone name (Incus naming
		// rules: the .block sibling and custom volumes must share the
		// instance name).
		finalBase := strings.Replace(part.DstBaseName, LXDBackupPrefix+vmID, cloneName, 1)
		parent := dstParent
		if part.Kind == "custom" {
			// Incus custom volumes are datasets named "<src>/custom/<project>_<vol>".
			parent = dstSource + "/custom"
			origVol := strings.TrimPrefix(part.DstBaseName, backupName+".")
			finalBase = "default_" + volRemap[origVol]
		}
		landingBase := "incoming-restore-" + finalBase
		landingDataset := parent + "/" + landingBase
		finalDataset := parent + "/" + finalBase

		// Clean up orphans from a prior FAILED restore of the same clone
		// name — both the landing and the final path. The clone name was
		// already proven to not be a registered Incus instance, so any
		// dataset here is a leftover, safe to destroy. Without the final-
		// path destroy, the post-syncoid rename fails "already exists".
		_ = exec.Command("sudo", "zfs", "destroy", "-r", landingDataset).Run()
		_ = exec.Command("sudo", "zfs", "destroy", "-r", finalDataset).Run()

		if logFn != nil {
			logFn(fmt.Sprintf("[%s] %s -> %s", part.Kind, part.SrcDataset, finalDataset))
		}
		if err := RunSyncoidLocal(ctx, part.SrcDataset, landingDataset, part.Recursive, logFn); err != nil {
			return err
		}
		if out, err := exec.Command("sudo", "zfs", "rename", landingDataset, finalDataset).CombinedOutput(); err != nil {
			return fmt.Errorf("zfs rename: %s", strings.TrimSpace(string(out)))
		}
		// Roll the dataset back to the user-picked snapshot so the
		// resulting clone reflects that point in time. Skipped when
		// snapshotName=="" (latest). Custom volumes are skipped — their
		// snapshot history is independent from the instance's
		// `incus snapshot create` cadence and that point-in-time name
		// won't exist on them.
		if snapshotName != "" && (part.Kind == "root-fs" || part.Kind == "root-blk") {
			rollbackTarget := finalDataset + "@" + snapshotName
			if logFn != nil {
				logFn("Rolling " + finalDataset + " back to @" + snapshotName)
			}
			if out, err := exec.Command("sudo", "zfs", "rollback", "-r", rollbackTarget).CombinedOutput(); err != nil {
				return fmt.Errorf("zfs rollback to @%s: %s", snapshotName, strings.TrimSpace(string(out)))
			}
		}
		// Destroy snapshot history on the cloned dataset — Incus refuses
		// a dataset whose snapshot count doesn't match backup.yaml's
		// `snapshots:` list, and we rewrite that list to [] just below.
		// Done on every part, root-fs + .block + custom, so they stay
		// aligned with each other and with backup.yaml.
		destroyDatasetSnapshots(finalDataset, logFn)
		// On the root-fs part: rewrite the embedded backup.yaml so its
		// pool, instance name, and (now-empty) snapshot list match the
		// destination dataset state. Without this `incus admin recover`
		// rejects the dataset.
		if part.Kind == "root-fs" {
			if logFn != nil {
				logFn(fmt.Sprintf("Rewriting backup.yaml: pool→%s, name %s→%s, snapshots→[]", dstDatastore, vmID, cloneName))
			}
			if err := LXDRewriteBackupYAMLForRestore(finalDataset, dstDatastore, dstSource, vmID, cloneName, volRemap); err != nil {
				if logFn != nil {
					logFn("rewrite backup.yaml: " + err.Error())
				}
			}
		}
	}

	// Trigger Incus recovery so the new dataset becomes a real instance.
	if logFn != nil {
		logFn("Running incus admin recover on " + dstDatastore)
	}
	out, err := LXDIncusAdminRecoverWithMask("")
	if logFn != nil {
		for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if ln != "" {
				logFn("  recover: " + ln)
			}
		}
	}
	if err != nil {
		return fmt.Errorf("incus admin recover: %s", strings.TrimSpace(string(out)))
	}
	// Clear volatile state copied from the original so the clone gets
	// fresh MAC addresses, UUIDs etc. — otherwise it collides with the
	// running source instance on start. Best-effort.
	resetCloneVolatileState(cloneName, logFn)
	return nil
}

// resetCloneVolatileState clears NIC-related `volatile.*` keys on the
// restored instance so Incus regenerates fresh MAC addresses on next
// start. Keys like volatile.uuid are NOT touched — Incus parses them on
// load and refuses an empty value.
//
// The targeted keys: anything matching volatile.*.hwaddr (NIC MACs),
// volatile.vsock_id (would collide with the source VM), and
// volatile.cloud-init.instance-id (so cloud-init treats the clone as a
// new instance).
func resetCloneVolatileState(instance string, logFn func(string)) {
	out, err := exec.Command("incus", "config", "show", instance).Output()
	if err != nil {
		return
	}
	inConfig := false
	for _, line := range strings.Split(string(out), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "config:" {
			inConfig = true
			continue
		}
		if !inConfig {
			continue
		}
		if len(line) > 0 && line[0] != ' ' && line[0] != '\t' {
			return
		}
		eq := strings.Index(trimmed, ":")
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:eq])
		if shouldResetVolatileKey(key) {
			if uerr := exec.Command("incus", "config", "unset", instance, key).Run(); uerr == nil {
				if logFn != nil {
					logFn("  unset " + key)
				}
			}
		}
	}
}

// shouldResetVolatileKey returns true for the narrow set of volatile keys
// that must NOT carry over from the original VM to the clone.
func shouldResetVolatileKey(key string) bool {
	if !strings.HasPrefix(key, "volatile.") {
		return false
	}
	if strings.HasSuffix(key, ".hwaddr") {
		return true
	}
	switch key {
	case "volatile.vsock_id",
		"volatile.cloud-init.instance-id",
		"volatile.last_state.power":
		return true
	}
	return false
}

// destroyDatasetSnapshots wipes all snapshots from `dataset`. Best-effort —
// errors are logged but not returned because restore can still succeed even
// if one stray snapshot lingers (Incus will refuse and the user can re-run).
func destroyDatasetSnapshots(dataset string, logFn func(string)) {
	out, err := exec.Command("zfs", "list", "-Hpt", "snapshot", "-o", "name", "-r", dataset).Output()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		snap := strings.TrimSpace(line)
		if snap == "" {
			continue
		}
		// Only this exact dataset's snapshots, not children.
		if !strings.HasPrefix(snap, dataset+"@") {
			continue
		}
		if dout, derr := exec.Command("sudo", "zfs", "destroy", snap).CombinedOutput(); derr != nil {
			if logFn != nil {
				logFn("destroy " + snap + ": " + strings.TrimSpace(string(dout)))
			}
		}
	}
}

// LXDCloneRestoreRemote performs a clone-restore where the source backup
// lives on a remote ZNAS peer reached over SSH.
//
// vmID is the original instance name on the peer (e.g. "vm-1"). It is
// needed to (a) probe for the VM's .block sibling zvol on the remote, and
// (b) rewrite backup.yaml so its `name:` field matches `cloneName` on
// recovery. `srcDataset` is the root-fs dataset path on the remote.
//
// `snapshotName` works the same as in LXDCloneRestoreLocal — empty means
// "latest"; otherwise the destination dataset is rolled back to that
// snapshot after syncoid pull.
func LXDCloneRestoreRemote(ctx context.Context, srcHost, srcUser, srcDataset, dstDatastore, cloneName, snapshotName, vmID string, logFn func(string)) error {
	if !lxdNameRe.MatchString(cloneName) {
		return fmt.Errorf("invalid clone name %q", cloneName)
	}
	if IsBackupInstanceName(cloneName) {
		return fmt.Errorf("clone name cannot start with %q", LXDBackupPrefix)
	}
	if _, err := LXDGetStatus(cloneName); err == nil {
		return fmt.Errorf("instance %q already exists", cloneName)
	}

	dstSource := getLXDPoolSource(dstDatastore)
	if dstSource == "" {
		return fmt.Errorf("destination datastore %q has no zfs source", dstDatastore)
	}

	// Heuristic to find the kind from the source dataset path: look for the
	// "virtual-machines" or "containers" segment.
	kind := "virtual-machines"
	if strings.Contains(srcDataset, "/containers/") {
		kind = "containers"
	}

	dstParent := dstSource + "/" + kind

	// Build the list of remote datasets to pull. Root-fs always; for VMs
	// the sibling ".block" zvol is the actual disk and must come too.
	type remotePart struct {
		src         string
		landingBase string
		finalBase   string
		isRootFS    bool
		recursive   bool
	}
	parts := []remotePart{{
		src:         srcDataset,
		landingBase: "incoming-restore-" + cloneName,
		finalBase:   cloneName,
		isRootFS:    true,
		recursive:   true,
	}}
	if kind == "virtual-machines" {
		parts = append(parts, remotePart{
			src:         srcDataset + ".block",
			landingBase: "incoming-restore-" + cloneName + ".block",
			finalBase:   cloneName + ".block",
			recursive:   false,
		})
	}

	_ = exec.Command("sudo", "zfs", "create", "-p", dstParent).Run()
	for _, p := range parts {
		landingDataset := dstParent + "/" + p.landingBase
		finalDataset := dstParent + "/" + p.finalBase
		// Clean up orphans from a prior FAILED restore of the same clone
		// name. The LXDGetStatus check above already proved <cloneName>
		// is not a registered Incus instance, so any dataset sitting at
		// the landing OR final path is a leftover and safe to destroy.
		// Without this, the post-syncoid `zfs rename landing→final`
		// fails with "dataset already exists".
		_ = exec.Command("sudo", "zfs", "destroy", "-r", landingDataset).Run()
		_ = exec.Command("sudo", "zfs", "destroy", "-r", finalDataset).Run()

		if logFn != nil {
			logFn(fmt.Sprintf("Pulling %s:%s -> %s", srcHost, p.src, landingDataset))
		}
		if err := RunSyncoidRestore(ctx, srcHost, srcUser, p.src, landingDataset, p.recursive, logFn); err != nil {
			// The .block zvol IS the VM's disk — restoring without it
			// yields a useless diskless instance. So a .block failure is
			// fatal, NOT a skip. The previous "(skip) non-root" path
			// silently produced corrupt restores when the backup on the
			// source side was itself incomplete (e.g. .block had no
			// snapshots). syncoid's "could not find any snapshots on
			// source" is the tell-tale of an incomplete backup.
			if !p.isRootFS && strings.Contains(err.Error(), "could not find any snapshots") {
				return fmt.Errorf("the backup on the source is incomplete — its disk image (%s) has no snapshots. "+
					"Re-run the backup of this VM to that destination, then retry the restore. (%v)", p.src, err)
			}
			return err
		}
		if out, err := exec.Command("sudo", "zfs", "rename", landingDataset, finalDataset).CombinedOutput(); err != nil {
			return fmt.Errorf("zfs rename: %s", strings.TrimSpace(string(out)))
		}
		if snapshotName != "" {
			rollbackTarget := finalDataset + "@" + snapshotName
			if logFn != nil {
				logFn("Rolling " + finalDataset + " back to @" + snapshotName)
			}
			if out, err := exec.Command("sudo", "zfs", "rollback", "-r", rollbackTarget).CombinedOutput(); err != nil {
				return fmt.Errorf("zfs rollback to @%s: %s", snapshotName, strings.TrimSpace(string(out)))
			}
		}
		destroyDatasetSnapshots(finalDataset, logFn)
		if p.isRootFS {
			if logFn != nil {
				logFn(fmt.Sprintf("Rewriting backup.yaml: pool→%s, name %s→%s, snapshots→[]", dstDatastore, vmID, cloneName))
			}
			// Remote clone-restore moves only the root (+.block); strip any
			// attached custom-volume disk device that isn't present.
			if err := LXDRewriteBackupYAMLForRestore(finalDataset, dstDatastore, dstSource, vmID, cloneName, map[string]string{}); err != nil {
				if logFn != nil {
					logFn("rewrite backup.yaml: " + err.Error())
				}
			}
		}
	}

	if logFn != nil {
		logFn("Running incus admin recover on " + dstDatastore)
	}
	out, err := LXDIncusAdminRecoverWithMask("")
	if logFn != nil {
		for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if ln != "" {
				logFn("  recover: " + ln)
			}
		}
	}
	if err != nil {
		return fmt.Errorf("incus admin recover: %s", strings.TrimSpace(string(out)))
	}
	// Clear volatile.eth0.hwaddr etc so the clone gets fresh values on
	// next start instead of colliding with the source VM's MAC.
	resetCloneVolatileState(cloneName, logFn)
	return nil
}
