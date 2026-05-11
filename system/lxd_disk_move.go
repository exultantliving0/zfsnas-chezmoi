package system

// Per-disk storage move (v6.5.37).
//
// The Related Objects card in the instance detail page now exposes a
// per-disk burger menu with a "Move" action. This file implements the
// host-side mechanics. Two cases, dispatched on `is_root`:
//
//   • Root disk → `incus move <instance> --storage <target>` moves the
//     entire instance (root + any custom volumes living in the same
//     storage pool). The user picked the root, so that's the action they
//     intended; explicitly moving "just the root" is not a thing Incus
//     supports because the root is the instance's storage anchor.
//
//   • Non-root disk → `incus storage volume move <srcPool>/<vol>
//     <targetPool>/<vol>` moves just that volume. Incus rebinds the
//     instance's disk device to the new pool automatically. Works only
//     when the instance is stopped (the UI greys the menu otherwise).
//
// Both code paths shell out to the `incus` CLI with an exec.CommandContext
// so the cancel handler in handlers/disk_move.go can kill the in-flight
// transfer when the user clicks ✕ in the activity bar.
//
// Output is line-tee'd to `logFn` so the progress endpoint can stream it
// back to the modal terminal — same pattern as proxmox_import.go.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// LXDInstanceDiskRef is the minimal subset of an instance's expanded_devices
// entry we need to dispatch a move. Populated by LookupInstanceDisk.
type LXDInstanceDiskRef struct {
	IsRoot bool
	Pool   string // source storage pool
	Source string // volume name within the source pool (for non-root disks)
}

// LookupInstanceDisk inspects an instance's expanded_devices for the named
// disk device and returns its pool/source. Returns an error if the device
// doesn't exist or isn't a `type: disk`. Used by the move start handler to
// validate the request and to pick the right `incus` subcommand.
func LookupInstanceDisk(instance, diskName string) (*LXDInstanceDiskRef, error) {
	if !lxdNameRe.MatchString(instance) {
		return nil, fmt.Errorf("invalid instance name")
	}
	out, err := exec.Command("incus", "query", "/1.0/instances/"+instance).Output()
	if err != nil {
		return nil, fmt.Errorf("incus query: %w", err)
	}
	var resp struct {
		ExpandedDevices map[string]map[string]string `json:"expanded_devices"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parse instance config: %w", err)
	}
	dev, ok := resp.ExpandedDevices[diskName]
	if !ok {
		return nil, fmt.Errorf("device %q not found on instance %q", diskName, instance)
	}
	if dev["type"] != "disk" {
		return nil, fmt.Errorf("device %q is not a disk", diskName)
	}
	return &LXDInstanceDiskRef{
		IsRoot: dev["path"] == "/", // Incus marks the root disk with path=/
		Pool:   dev["pool"],
		Source: dev["source"],
	}, nil
}

// LXDMoveInstanceDisk migrates the named disk to targetPool. The instance
// MUST be stopped — the handler enforces this via LXDGetInstanceStatus
// before invoking us. logFn receives each line of `incus` stdout/stderr so
// the front-end terminal can show live progress; pass nil to discard.
//
// Cancellation: ctx is forwarded to exec.CommandContext, so a Done ctx
// SIGKILLs the underlying `incus` process. Incus then rolls back the
// partial move on its own.
func LXDMoveInstanceDisk(ctx context.Context, instance, diskName, targetPool string, logFn func(string)) error {
	if !lxdNameRe.MatchString(instance) {
		return fmt.Errorf("invalid instance name")
	}
	if strings.TrimSpace(targetPool) == "" {
		return fmt.Errorf("target pool is required")
	}
	ref, err := LookupInstanceDisk(instance, diskName)
	if err != nil {
		return err
	}
	if ref.Pool == targetPool {
		return fmt.Errorf("disk %q is already on pool %q", diskName, targetPool)
	}

	var cmd *exec.Cmd
	switch {
	case ref.IsRoot:
		// Root disk move: take the whole instance with us.
		if logFn != nil {
			logFn(fmt.Sprintf("Moving instance %q (root disk) from %q to %q…", instance, ref.Pool, targetPool))
		}
		cmd = exec.CommandContext(ctx, "incus", "move", instance, "--storage", targetPool)

	default:
		// Non-root custom volume move. The volume keeps its source name —
		// only the pool changes — so the instance's disk device entry
		// (source: <volname>) doesn't need rewriting.
		if ref.Source == "" {
			return fmt.Errorf("disk %q has no source volume — cannot move", diskName)
		}
		src := ref.Pool + "/" + ref.Source
		dst := targetPool + "/" + ref.Source
		if logFn != nil {
			logFn(fmt.Sprintf("Moving volume %q to %q…", src, dst))
		}
		cmd = exec.CommandContext(ctx, "incus", "storage", "volume", "move", src, dst)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = cmd.Stdout // fold stderr into stdout so logFn sees both in order
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start incus: %w", err)
	}

	// Stream output line-by-line. `incus` writes progress to a single line
	// it keeps overwriting via \r; the bufio.Scanner default tokenizer
	// only splits on \n, so each refreshed progress line still arrives.
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		if logFn != nil {
			logFn(scanner.Text())
		}
	}
	// scanner.Err() can be io.EOF or a genuine read error. Either way the
	// authoritative success/fail is cmd.Wait().
	_ = io.EOF // keep import; bufio doc references EOF behavior

	if err := cmd.Wait(); err != nil {
		// Context-canceled exits look like signal kills — surface a
		// recognizable cancellation error so the handler can audit-log it
		// distinctly from a real failure.
		if ctx.Err() != nil {
			return fmt.Errorf("canceled: %w", ctx.Err())
		}
		return fmt.Errorf("incus move failed: %w", err)
	}
	if logFn != nil {
		logFn("Move complete.")
	}
	return nil
}
