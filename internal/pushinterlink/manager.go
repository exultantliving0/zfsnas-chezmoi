package pushinterlink

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"
)

// State represents the lifecycle state of a push job.
type State string

const (
	StatePending   State = "pending"
	StateRunning   State = "running"
	StateDone      State = "done"
	StateError     State = "error"
	StateCancelled State = "cancelled"
)

// Job tracks one ZFS-send-over-SSH push operation.
type Job struct {
	ID           string     `json:"id"`
	SnapshotName string     `json:"snapshot_name"`
	DestDataset  string     `json:"dest_dataset"`
	RemoteServer string     `json:"remote_server"`
	State        State      `json:"state"`
	BytesSent    int64      `json:"bytes_sent"`
	TotalBytes   int64      `json:"total_bytes"`
	StartedAt    time.Time  `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	ErrorMsg     string     `json:"error_msg,omitempty"`
	StartedBy    string     `json:"started_by"`
	TempSnapshot string     `json:"temp_snapshot,omitempty"` // set for dataset-push jobs; cleaned up on ForceCancel
	cancel       context.CancelFunc
}

// Manager is the in-process registry of push jobs (lives as long as the process).
type Manager struct {
	mu   sync.Mutex
	jobs map[string]*Job
}

// Default is the singleton job manager.
var Default = &Manager{jobs: make(map[string]*Job)}

func genID() string {
	b := make([]byte, 6)
	rand.Read(b) //nolint:errcheck
	return "push-" + hex.EncodeToString(b)
}

// Register adds a new job and returns it.
func (m *Manager) Register(snapshot, destDataset, remoteServer, startedBy string, cancel context.CancelFunc) *Job {
	j := &Job{
		ID:           genID(),
		SnapshotName: snapshot,
		DestDataset:  destDataset,
		RemoteServer: remoteServer,
		State:        StatePending,
		StartedAt:    time.Now(),
		StartedBy:    startedBy,
		cancel:       cancel,
	}
	m.mu.Lock()
	m.jobs[j.ID] = j
	m.mu.Unlock()
	return j
}

// SetRunning marks the job as running and records the total byte estimate.
func (m *Manager) SetRunning(id string, totalBytes int64) {
	m.mu.Lock()
	if j, ok := m.jobs[id]; ok {
		j.State = StateRunning
		if totalBytes > 0 {
			j.TotalBytes = totalBytes
		}
	}
	m.mu.Unlock()
}

// UpdateProgress records bytes sent so far.
func (m *Manager) UpdateProgress(id string, sent int64) {
	m.mu.Lock()
	if j, ok := m.jobs[id]; ok {
		j.BytesSent = sent
	}
	m.mu.Unlock()
}

// Finish marks the job done, errored, or cancelled and schedules a prune.
func (m *Manager) Finish(id string, err error) {
	m.mu.Lock()
	if j, ok := m.jobs[id]; ok {
		now := time.Now()
		j.EndedAt = &now
		if err == nil {
			j.State = StateDone
		} else if errors.Is(err, context.Canceled) {
			j.State = StateCancelled
		} else {
			j.State = StateError
			j.ErrorMsg = err.Error()
		}
	}
	m.mu.Unlock()
	go m.prune()
}

// SetTempSnapshot records the temporary snapshot name for a dataset-push job
// so ForceCancel can clean it up even if the goroutine is stuck.
func (m *Manager) SetTempSnapshot(id, snap string) {
	m.mu.Lock()
	if j, ok := m.jobs[id]; ok {
		j.TempSnapshot = snap
	}
	m.mu.Unlock()
}

// GetTempSnapshot returns the temp snapshot name associated with a job (empty if none).
func (m *Manager) GetTempSnapshot(id string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if j, ok := m.jobs[id]; ok {
		return j.TempSnapshot
	}
	return ""
}

// Cancel signals the job's context to cancel. The job state is updated by the
// goroutine when it unblocks. Use ForceCancel when the goroutine may be stuck.
func (m *Manager) Cancel(id string) bool {
	m.mu.Lock()
	j, ok := m.jobs[id]
	m.mu.Unlock()
	if !ok {
		return false
	}
	if j.cancel != nil {
		j.cancel()
	}
	return true
}

// ForceCancel immediately marks the job as cancelled in the manager (so the UI
// stops showing it) and signals the context. The goroutine may still be alive
// if the OS process is in an uninterruptible wait; the caller should clean up
// any temporary snapshot separately.
func (m *Manager) ForceCancel(id string) (snap string, ok bool) {
	m.mu.Lock()
	j, found := m.jobs[id]
	if !found {
		m.mu.Unlock()
		return "", false
	}
	cancel := j.cancel
	snap = j.TempSnapshot
	if j.State == StatePending || j.State == StateRunning {
		now := time.Now()
		j.State = StateCancelled
		j.EndedAt = &now
		j.cancel = nil
	}
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	go m.prune()
	return snap, true
}

// List returns a copy of all jobs sorted by start time (newest first).
func (m *Manager) List() []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		cp := *j
		cp.cancel = nil
		out = append(out, cp)
	}
	sort.Slice(out, func(i, k int) bool { return out[i].StartedAt.After(out[k].StartedAt) })
	return out
}

// prune removes finished jobs beyond the most recent 20.
func (m *Manager) prune() {
	m.mu.Lock()
	defer m.mu.Unlock()
	type fin struct {
		id  string
		end time.Time
	}
	var finished []fin
	for id, j := range m.jobs {
		if j.State != StatePending && j.State != StateRunning && j.EndedAt != nil {
			finished = append(finished, fin{id, *j.EndedAt})
		}
	}
	if len(finished) <= 20 {
		return
	}
	sort.Slice(finished, func(i, k int) bool { return finished[i].end.Before(finished[k].end) })
	for _, f := range finished[:len(finished)-20] {
		delete(m.jobs, f.id)
	}
}
