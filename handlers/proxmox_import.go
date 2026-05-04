package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
	"zfsnas/internal/audit"
	"zfsnas/system"
)

// pxiJob tracks an async Proxmox import operation for a single VM.
type pxiJob struct {
	mu           sync.Mutex
	Status       string `json:"status"` // "queued" | "running" | "done" | "error" | "canceled"
	Error        string `json:"error,omitempty"`
	Lines        []string
	TotalBytes   int64
	BytesWritten int64
	VMName       string `json:"vm_name"`
	cancelFn     context.CancelFunc
}

func (j *pxiJob) cancel() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Status == "queued" || j.Status == "running" {
		j.Status = "canceled"
		if j.cancelFn != nil {
			j.cancelFn()
		}
	}
}

var (
	pxiJobs sync.Map                 // job_id → *pxiJob
	pxiSem  = make(chan struct{}, 4) // max 4 parallel imports
)

// HandleProxmoxImportToolsStatus reports whether the import-time tools are
// installed. ntfs-3g + python3-hivex were added in v6.5.2 for the Windows
// boot-state repair (fixUEFIWindows in system/proxmox_import.go).
// GET /api/lxd/proxmox-import/tools-status
func HandleProxmoxImportToolsStatus(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]bool{
		"sshpass":  system.SshpassAvailable(),
		"qemu_img": system.QemuImgAvailable(),
		"ntfsfix":  system.NtfsfixAvailable(),
		"hivex":    system.HivexAvailable(),
	})
}

// HandleProxmoxImportList SSHes to the remote Proxmox host and returns
// the list of VMs with their config (CPU, RAM, disks, NICs, status).
// POST /api/lxd/proxmox-import/list
func HandleProxmoxImportList(w http.ResponseWriter, r *http.Request) {
	if !system.SshpassAvailable() {
		jsonErr(w, http.StatusServiceUnavailable,
			"sshpass is not installed; install it via Prerequisites")
		return
	}

	var req struct {
		Host     string `json:"host"`
		User     string `json:"user"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Host == "" || req.User == "" || req.Password == "" {
		jsonErr(w, http.StatusBadRequest, "host, user and password are required")
		return
	}

	conn := system.ProxmoxSSHConn{
		Host:     req.Host,
		User:     req.User,
		Password: req.Password,
	}
	vms, err := system.ListProxmoxVMs(conn)
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	jsonOK(w, vms)
}

// HandleProxmoxImportStart starts the async import of selected Proxmox VMs.
// Each VM gets its own job with independent progress tracking; up to 4 run in parallel.
// POST /api/lxd/proxmox-import/start
func HandleProxmoxImportStart(w http.ResponseWriter, r *http.Request) {
	var req system.ProxmoxImportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.VMs) == 0 {
		jsonErr(w, http.StatusBadRequest, "no VMs selected")
		return
	}
	if req.LocalBridge == "" {
		jsonErr(w, http.StatusBadRequest, "local_bridge is required")
		return
	}
	if req.StoragePool == "" {
		jsonErr(w, http.StatusBadRequest, "storage_pool is required")
		return
	}

	sess := MustSession(r)
	conn := system.ProxmoxSSHConn{
		Host:     req.Host,
		User:     req.User,
		Password: req.Password,
	}

	type jobEntry struct {
		JobID  string `json:"job_id"`
		VMName string `json:"vm_name"`
	}
	var entries []jobEntry

	for _, vm := range req.VMs {
		jobID := fmt.Sprintf("pxi-%d-%d", vm.VMID, time.Now().UnixNano())
		ctx, cancelFn := context.WithCancel(context.Background())

		var totalBytes int64
		for _, d := range vm.Disks {
			totalBytes += d.SizeBytes
		}

		job := &pxiJob{
			Status:     "queued",
			TotalBytes: totalBytes,
			VMName:     vm.Name,
			cancelFn:   cancelFn,
		}
		pxiJobs.Store(jobID, job)
		entries = append(entries, jobEntry{JobID: jobID, VMName: vm.Name})

		vmCopy := vm
		go func(jobID string, vm system.ProxmoxVM, ctx context.Context, job *pxiJob) {
			// Wait for a semaphore slot (queued while all 4 slots are busy).
			select {
			case pxiSem <- struct{}{}:
				// acquired
			case <-ctx.Done():
				// canceled while queued
				job.mu.Lock()
				if job.Status == "queued" {
					job.Status = "canceled"
				}
				job.mu.Unlock()
				return
			}
			defer func() { <-pxiSem }()

			// Check if canceled between queue and start.
			if ctx.Err() != nil {
				job.mu.Lock()
				if job.Status != "canceled" {
					job.Status = "canceled"
				}
				job.mu.Unlock()
				return
			}

			job.mu.Lock()
			job.Status = "running"
			job.mu.Unlock()

			logCh := make(chan string, 256)
			go func() {
				for line := range logCh {
					job.mu.Lock()
					job.Lines = append(job.Lines, line)
					job.mu.Unlock()
				}
			}()

			progressFn := func(n int64) { atomic.AddInt64(&job.BytesWritten, n) }

			err := system.ImportProxmoxVM(ctx, conn, vm, req, logCh, progressFn)
			close(logCh)

			result := audit.ResultOK
			details := ""
			job.mu.Lock()
			if err != nil {
				if job.Status != "canceled" {
					job.Status = "error"
					job.Error = err.Error()
				}
				result = audit.ResultError
				details = err.Error()
			} else {
				job.Status = "done"
			}
			job.mu.Unlock()

			audit.Log(audit.Entry{
				User:    sess.Username,
				Role:    sess.Role,
				Action:  audit.ActionProxmoxImport,
				Target:  vm.Name,
				Result:  result,
				Details: details,
			})
		}(jobID, vmCopy, ctx, job)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(entries)
}

// HandleProxmoxImportProgress returns accumulated log lines and status for a job.
// GET /api/lxd/proxmox-import/progress?job_id=<id>
func HandleProxmoxImportProgress(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("job_id")
	val, ok := pxiJobs.Load(id)
	if !ok {
		jsonErr(w, http.StatusNotFound, "job not found")
		return
	}
	job := val.(*pxiJob)
	job.mu.Lock()
	status := job.Status
	errMsg := job.Error
	lines := job.Lines
	total := job.TotalBytes
	vmName := job.VMName
	job.mu.Unlock()

	written := atomic.LoadInt64(&job.BytesWritten)
	progress := float64(0)
	if total > 0 {
		progress = float64(written) / float64(total)
		if progress > 1.0 {
			progress = 1.0
		}
	}

	jsonOK(w, map[string]interface{}{
		"status":   status,
		"error":    errMsg,
		"lines":    lines,
		"progress": progress,
		"vm_name":  vmName,
	})
}

// HandleProxmoxImportCancel cancels a queued or running import job.
// POST /api/lxd/proxmox-import/cancel?job_id=<id>
func HandleProxmoxImportCancel(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("job_id")
	val, ok := pxiJobs.Load(id)
	if !ok {
		jsonErr(w, http.StatusNotFound, "job not found")
		return
	}
	job := val.(*pxiJob)
	job.cancel()
	jsonOK(w, map[string]bool{"ok": true})
}
