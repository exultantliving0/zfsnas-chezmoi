package system

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// RunLocalReplication sends an existing snapshot to a local destination dataset
// using zfs send | zfs receive without SSH.
//
// sourceDataset is the source dataset (e.g. "tank/data").
// fullSnapName is the full snapshot name already created (e.g. "tank/data@auto-20240323").
// destDataset is the local destination (e.g. "backup/data").
// lastSnap is the suffix of the last successful local replication snapshot (enables incremental sends).
// Returns the snapshot suffix on success so the caller can store it as LastLocalReplSnap.
func RunLocalReplication(sourceDataset, fullSnapName, destDataset, lastSnap string, recursive, compressed bool, send func(string)) (snapSuffix string, err error) {
	at := strings.LastIndex(fullSnapName, "@")
	if at < 0 {
		return "", fmt.Errorf("invalid snapshot name (missing @): %s", fullSnapName)
	}
	snapSuffix = fullSnapName[at+1:]

	send(fmt.Sprintf("Local replication: %s → %s", fullSnapName, destDataset))

	// Build zfs send args.
	sendArgs := []string{"send"}
	if recursive {
		sendArgs = append(sendArgs, "-R")
	}
	if compressed {
		sendArgs = append(sendArgs, "-c")
	}
	if lastSnap != "" {
		sendArgs = append(sendArgs, "-I", sourceDataset+"@"+lastSnap)
	}
	sendArgs = append(sendArgs, fullSnapName)

	// Build zfs receive args.
	recvArgs := []string{"receive", "-F", destDataset}

	send(fmt.Sprintf("sudo zfs %s | sudo zfs %s", strings.Join(sendArgs, " "), strings.Join(recvArgs, " ")))
	send("─────────────────────────────────────────")

	zfsSendCmd := exec.Command("sudo", append([]string{"zfs"}, sendArgs...)...)
	zfsRecvCmd := exec.Command("sudo", append([]string{"zfs"}, recvArgs...)...)

	// Wire send stdout → receive stdin via OS pipe.
	dataR, dataW, pipeErr := os.Pipe()
	if pipeErr != nil {
		return "", fmt.Errorf("pipe: %w", pipeErr)
	}
	zfsSendCmd.Stdout = dataW
	zfsRecvCmd.Stdin = dataR

	var sendStderr, recvStderr bytes.Buffer
	zfsSendCmd.Stderr = &sendStderr
	zfsRecvCmd.Stderr = &recvStderr

	// Start receiver first so it is ready before the stream begins.
	if startErr := zfsRecvCmd.Start(); startErr != nil {
		dataR.Close()
		dataW.Close()
		return "", fmt.Errorf("start zfs receive: %w", startErr)
	}
	if startErr := zfsSendCmd.Start(); startErr != nil {
		dataW.Close()
		dataR.Close()
		zfsRecvCmd.Process.Kill()
		zfsRecvCmd.Wait()
		return "", fmt.Errorf("start zfs send: %w", startErr)
	}

	// Close parent copies — each child process has its own fd.
	dataW.Close()
	dataR.Close()

	// Wait for send first; its exit closes the pipe which signals receive EOF.
	sendWaitErr := zfsSendCmd.Wait()
	recvWaitErr := zfsRecvCmd.Wait()

	for _, l := range strings.Split(strings.TrimSpace(sendStderr.String()), "\n") {
		if strings.TrimSpace(l) != "" {
			send(l)
		}
	}
	for _, l := range strings.Split(strings.TrimSpace(recvStderr.String()), "\n") {
		if strings.TrimSpace(l) != "" {
			send(l)
		}
	}

	if sendWaitErr != nil {
		return "", fmt.Errorf("zfs send failed: %w: %s", sendWaitErr, strings.TrimSpace(sendStderr.String()))
	}
	if recvWaitErr != nil {
		return "", fmt.Errorf("zfs receive failed: %w: %s", recvWaitErr, strings.TrimSpace(recvStderr.String()))
	}

	return snapSuffix, nil
}
