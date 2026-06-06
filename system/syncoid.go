package system

// syncoid integration for the VM/Container backup feature (v6.5.19+).
//
// syncoid is part of the sanoid Debian/Ubuntu package. It sends a ZFS-aware
// incremental snapshot stream between two datasets, either locally or over
// SSH. We rely on it for backups so we don't have to reimplement the snapshot
// bookkeeping ourselves.
//
// Two transports are supported here:
//   • Local — both source and destination dataset live on this host.
//     RunSyncoidLocal: syncoid <src> <dst>
//   • Remote (push) — destination is on a peer ZNAS reached over SSH using
//     the same key plumbing as push-interlink.
//     RunSyncoidRemote: syncoid <src> root@<host>:<dst>
//   • Remote (pull) — for Restore/Clone from a remote backup.
//     RunSyncoidRestore: syncoid root@<host>:<src> <dst>
//
// Progress lines emitted on stdout/stderr are forwarded to a caller-supplied
// logFn so the activity-bar job pane can show what's happening.

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

// PickReachableSSHHost returns the first host from `candidates` that
// answers an SSH key-auth echo test as `user`, or "" when none do.
// `urlHost` is appended as a last-resort candidate (the InterLink URL
// hostname) so behaviour degrades gracefully on peers that don't yet
// advertise SSHHosts.
//
// This solves the reverse-proxy problem: the InterLink URL may be an
// HTTPS endpoint behind a proxy, unusable as an SSH transport. The peer
// advertises its real IPs via RemotePoolsResponse.SSHHosts; we probe
// each and use the first that actually authenticates.
func PickReachableSSHHost(candidates []string, urlHost, user string) string {
	tried := map[string]bool{}
	probe := func(h string) bool {
		if h == "" || tried[h] {
			return false
		}
		tried[h] = true
		args := []string{"-i", zfsnasSSHKey(),
			"-o", "BatchMode=yes",
			"-o", "StrictHostKeyChecking=accept-new",
			"-o", "ConnectTimeout=5",
			user + "@" + h, "true"}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, "ssh", args...).Run() == nil
	}
	for _, h := range candidates {
		if probe(h) {
			return h
		}
	}
	if probe(urlHost) {
		return urlHost
	}
	return ""
}

// zfsnasSSHKey returns the path to the zfsnas-user SSH private key (the
// same key already established by push-interlink: `~/.ssh/id_ed25519` of
// the service user). syncoid runs via sudo so it loses the zfsnas user's
// home and can't find the key implicitly — we must pass it explicitly via
// --sshoption=IdentityFile=…
func zfsnasSSHKey() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return filepath.Join(u.HomeDir, ".ssh", "id_ed25519")
}

// SyncoidPrereqsInstalled returns true when /usr/sbin/syncoid is on PATH.
func SyncoidPrereqsInstalled() bool {
	_, err := exec.LookPath("syncoid")
	return err == nil
}

// InstallSyncoid runs apt-get to install the sanoid package (which contains
// the syncoid binary). Combined output is returned for the install-progress
// log.
func InstallSyncoid() ([]byte, error) {
	return exec.Command("sudo", "apt-get", "install", "-y", "-q", "sanoid").CombinedOutput()
}

// RunSyncoidLocal replicates src → dst locally (no SSH). recursive=true sends
// child datasets (the VM zvol partition for example).
//
// ownSnap controls snapshotting. The instance's root-fs and .block parts are
// snapshotted up-front by `incus snapshot create` (one atomic, ZNAS-named
// snapshot), so they pass ownSnap=false → --no-sync-snap, and syncoid sends
// that existing snapshot. Attached custom-volume vdisks are NOT covered by an
// incus instance snapshot, so they pass ownSnap=true: syncoid creates and
// auto-prunes its own sync snapshot, which also maintains the incremental
// anchor across fires with no extra bookkeeping.
func RunSyncoidLocal(ctx context.Context, src, dst string, recursive, ownSnap bool, logFn func(string)) error {
	args := []string{"syncoid", "--no-privilege-elevation"}
	if !ownSnap {
		args = append(args, "--no-sync-snap")
	}
	if recursive {
		args = append(args, "--recursive")
	}
	args = append(args, src, dst)
	return runSyncoid(ctx, args, logFn)
}

// RunSyncoidRemote replicates a local source dataset to a peer ZNAS over SSH.
// host = remote IP/hostname; user = remote unix user (matches push-interlink
// process user); dst = remote dataset path.
func RunSyncoidRemote(ctx context.Context, src, host, remoteUser, dst string, recursive, ownSnap bool, logFn func(string)) error {
	args := []string{"syncoid", "--no-privilege-elevation",
		"--sshoption=StrictHostKeyChecking=accept-new",
		"--sshoption=BatchMode=yes"}
	if !ownSnap {
		// root-fs/.block carry an incus-made snapshot already; custom vdisks
		// don't, so they let syncoid create+prune its own (see RunSyncoidLocal).
		args = append(args, "--no-sync-snap")
	}
	// syncoid is invoked under sudo, which means ssh otherwise looks in
	// /root/.ssh; the trusted interlink key lives in the zfsnas user's
	// home, so pass it explicitly.
	if key := zfsnasSSHKey(); key != "" {
		args = append(args, "--sshkey="+key)
	}
	if recursive {
		args = append(args, "--recursive")
	}
	target := dst
	if remoteUser != "" {
		target = remoteUser + "@" + host + ":" + dst
	} else {
		target = host + ":" + dst
	}
	args = append(args, src, target)
	return runSyncoid(ctx, args, logFn)
}

// RunSyncoidRestore pulls a remote dataset back to a local destination
// (inverse of RunSyncoidRemote). Used by the Restore/Clone flow when the
// chosen backup lives on a remote datastore.
func RunSyncoidRestore(ctx context.Context, srcHost, srcUser, srcDataset, dstDataset string, recursive bool, logFn func(string)) error {
	args := []string{"syncoid", "--no-privilege-elevation", "--no-sync-snap",
		"--sshoption=StrictHostKeyChecking=accept-new",
		"--sshoption=BatchMode=yes"}
	if key := zfsnasSSHKey(); key != "" {
		args = append(args, "--sshkey="+key)
	}
	if recursive {
		args = append(args, "--recursive")
	}
	source := srcDataset
	if srcUser != "" {
		source = srcUser + "@" + srcHost + ":" + srcDataset
	} else {
		source = srcHost + ":" + srcDataset
	}
	args = append(args, source, dstDataset)
	return runSyncoid(ctx, args, logFn)
}

// runSyncoid invokes syncoid via sudo, streams stdout+stderr to logFn, and
// returns a nice error when it exits non-zero or the context is canceled.
func runSyncoid(ctx context.Context, args []string, logFn func(string)) error {
	if logFn != nil {
		logFn("$ sudo " + strings.Join(args, " "))
	}
	cmd := exec.CommandContext(ctx, "sudo", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("pipe stdout: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("pipe stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start syncoid: %w", err)
	}

	done := make(chan struct{}, 2)
	scan := func(rc io.Reader) {
		s := bufio.NewScanner(rc)
		s.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for s.Scan() {
			line := strings.TrimRight(s.Text(), "\r\n")
			if line == "" {
				continue
			}
			if logFn != nil {
				logFn(line)
			}
		}
		done <- struct{}{}
	}
	go scan(stdout)
	go scan(stderr)
	<-done
	<-done

	if err := cmd.Wait(); err != nil {
		if ctx.Err() == context.Canceled {
			return context.Canceled
		}
		return fmt.Errorf("syncoid: %w", err)
	}
	return nil
}
