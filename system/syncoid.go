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
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
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
// WriteInterlinkKnownHosts writes a throwaway known_hosts file pinning each of
// `hosts` to every key in `hostKeys` (each "<type> <base64>"), and returns its
// path. The caller passes this path to PickReachableSSHHost / the syncoid
// runners so the peer's SSH host identity is verified against keys fetched over
// the authenticated InterLink channel — not blind TOFU — and so a re-keyed peer
// self-heals (the file is rebuilt from the current keys on every transfer).
// Returns "" when hostKeys is empty (older peer that doesn't advertise keys);
// callers then fall back to the legacy accept-new behaviour. The caller is
// responsible for os.Remove-ing the returned path.
func WriteInterlinkKnownHosts(hosts, hostKeys []string) string {
	if len(hostKeys) == 0 || len(hosts) == 0 {
		return ""
	}
	var b strings.Builder
	for _, h := range hosts {
		if strings.TrimSpace(h) == "" {
			continue
		}
		for _, k := range hostKeys {
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
			b.WriteString(h)
			b.WriteByte(' ')
			b.WriteString(k)
			b.WriteByte('\n')
		}
	}
	f, err := os.CreateTemp("", "znas-interlink-knownhosts-*")
	if err != nil {
		return ""
	}
	// World-readable: syncoid runs under sudo (root) while the probe runs as the
	// zfsnas service user; both must be able to read it.
	_ = f.Chmod(0o644)
	if _, err := f.WriteString(b.String()); err != nil {
		f.Close()
		os.Remove(f.Name())
		return ""
	}
	f.Close()
	return f.Name()
}

// sshHostKeyOpts returns the ssh -o options selecting host-key verification.
// With a managed known_hosts file (peer advertised its keys) we verify strictly
// against it and ignore the system/user known_hosts entirely (which sidesteps a
// stale entry in /root/.ssh/known_hosts left over from a previous host key).
// Without one, we keep the legacy lenient accept-new behaviour.
func sshHostKeyOpts(knownHostsFile string) []string {
	if knownHostsFile != "" {
		return []string{
			"UserKnownHostsFile=" + knownHostsFile,
			"GlobalKnownHostsFile=/dev/null",
			"StrictHostKeyChecking=yes",
		}
	}
	return []string{"StrictHostKeyChecking=accept-new"}
}

// PickReachableSSHHost returns the first host from `candidates` that
// authenticates as `user`. When knownHostsFile is non-empty the peer's host key
// is verified against it (see WriteInterlinkKnownHosts); otherwise accept-new.
func PickReachableSSHHost(candidates []string, urlHost, user, knownHostsFile string) string {
	tried := map[string]bool{}
	probe := func(h string) bool {
		if h == "" || tried[h] {
			return false
		}
		tried[h] = true
		args := []string{"-i", zfsnasSSHKey(),
			"-o", "BatchMode=yes",
			"-o", "ConnectTimeout=5"}
		for _, o := range sshHostKeyOpts(knownHostsFile) {
			args = append(args, "-o", o)
		}
		args = append(args, user+"@"+h, "true")
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
	return binaryInstalled("syncoid")
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
func RunSyncoidRemote(ctx context.Context, src, host, remoteUser, dst string, recursive, ownSnap bool, knownHostsFile string, logFn func(string)) error {
	args := []string{"syncoid", "--no-privilege-elevation",
		"--sshoption=BatchMode=yes"}
	for _, o := range sshHostKeyOpts(knownHostsFile) {
		args = append(args, "--sshoption="+o)
	}
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
func RunSyncoidRestore(ctx context.Context, srcHost, srcUser, srcDataset, dstDataset string, recursive bool, knownHostsFile string, logFn func(string)) error {
	args := []string{"syncoid", "--no-privilege-elevation", "--no-sync-snap",
		"--sshoption=BatchMode=yes"}
	for _, o := range sshHostKeyOpts(knownHostsFile) {
		args = append(args, "--sshoption="+o)
	}
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
	// Run syncoid in its own process group so cancellation can take down the
	// WHOLE pipeline (sudo → syncoid → sh -c 'zfs send | pv | mbuffer | ssh').
	// exec.CommandContext's default only SIGKILLs the direct child (sudo),
	// which orphans the zfs-send pipeline underneath it — a stuck/abandoned
	// backup that keeps hammering the disks. With Setpgid the child is its own
	// group leader (pgid == pid), so signalling -pid hits every descendant.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// SIGTERM the group first for a clean teardown, then SIGKILL to be sure.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second
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
		// Break on BOTH '\n' and '\r' so pv's carriage-return-terminated
		// progress updates ("4.17GiB … 25% ETA 0:01:30") stream through as
		// they happen instead of being swallowed until the next newline. This
		// is what gives the activity bar a live percentage.
		s.Split(scanLinesAndCR)
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

// scanLinesAndCR is a bufio.SplitFunc that splits on either '\n' or '\r'. The
// stdlib bufio.ScanLines only breaks on '\n', so pv's progress updates (which
// it rewrites in place using a bare '\r') would never surface until syncoid
// printed a newline. Splitting on '\r' too lets each progress refresh through.
func scanLinesAndCR(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i, b := range data {
		if b == '\n' || b == '\r' {
			return i + 1, data[:i], nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil // request more data
}

// ParseSyncoidPercent extracts a 0–100 progress percentage from a pv progress
// line (e.g. "4.17GiB 0:00:30 [142MiB/s] [===>      ] 25% ETA 0:01:30").
// Returns ok=false when the line carries no percentage.
func ParseSyncoidPercent(line string) (int, bool) {
	// Find a "NN%" token; pv right-pads the percentage so it always precedes
	// the literal '%'. Walk back over digits from each '%'.
	for i := 0; i < len(line); i++ {
		if line[i] != '%' {
			continue
		}
		j := i
		for j > 0 && line[j-1] >= '0' && line[j-1] <= '9' {
			j--
		}
		if j == i {
			continue // a lone '%' with no leading digits
		}
		n := 0
		for k := j; k < i; k++ {
			n = n*10 + int(line[k]-'0')
		}
		if n >= 0 && n <= 100 {
			return n, true
		}
	}
	return 0, false
}
