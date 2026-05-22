package handlers

// Per-instance backup feature (v6.5.19+).
//
// Endpoints exposed here drive the Feature 2 UI on the VM/Container page:
//   • GET /api/incus/instances/{name}/backups            — list backups (this host only; remote aggregation is in lxd_backups_all.go)
//   • GET/PUT/DELETE  /api/incus/instances/{name}/backup-schedule
//   • POST           /api/incus/instances/{name}/backup-now
//   • GET            /api/incus/backup-jobs/{job_id}/progress
//   • POST           /api/incus/backup-jobs/{job_id}/cancel

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"

	"zfsnas/internal/audit"
	"zfsnas/internal/config"
	"zfsnas/system"
)

// backupSchedMu guards in-memory edits of AppConfig.LXDBackupPolicies.
var backupSchedMu sync.Mutex

// --- Job registry (in-memory, restart-volatile — same trade-off as disk_move) ---

type lxdBackupJob struct {
	mu        sync.Mutex
	Status    string    `json:"status"` // "queued"|"running"|"done"|"error"|"canceled"
	Error     string    `json:"error,omitempty"`
	Lines     []string  `json:"lines"`
	Instance  string    `json:"instance"`
	DestKind  string    `json:"dest_kind"`
	DestHost  string    `json:"dest_host,omitempty"`
	DestPool  string    `json:"dest_pool"`
	StartedAt time.Time `json:"started_at"`
	cancelFn  context.CancelFunc
}

func (j *lxdBackupJob) appendLine(line string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if len(j.Lines) > 4000 {
		j.Lines = j.Lines[len(j.Lines)-3000:]
	}
	j.Lines = append(j.Lines, line)
}

func (j *lxdBackupJob) cancel() {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.Status == "queued" || j.Status == "running" {
		j.Status = "canceled"
		if j.cancelFn != nil {
			j.cancelFn()
		}
	}
}

var lxdBackupJobs sync.Map // job_id → *lxdBackupJob

// Allowed (every_n, unit) pairs for backup schedules. The "minute" tier has
// a 20-minute floor — any sub-20-minute period would whip syncoid into a
// tight loop. The other tiers mirror the snapshot schedule.
var allowedBackupUnits = map[string][]int{
	"minute": {20, 30},
	"hour":   {1, 2, 3, 4, 6, 8, 12},
	"day":    {1, 2, 3, 7, 14},
	"week":   {1, 2, 4},
	"month":  {1, 2, 3, 6, 12},
}

// HandleListInstanceBackups returns local backups that exist for the named
// instance. Remote backups are merged client-side by joining the cross-server
// aggregator's response.
// GET /api/incus/instances/{name}/backups
func HandleListInstanceBackups(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	resp := map[string]interface{}{
		"instance": name,
		"local":    listLocalBackupsForVM(name),
	}
	jsonOK(w, resp)
}

// listLocalBackupsForVM returns the local datastores hosting a bkup--<name>
// instance along with its snapshots. v6.5.19+: scans BOTH the workload
// layout (the v6.5.19+ canonical location) and the legacy Incus storage-
// pool location so existing deployments still surface their backups.
func listLocalBackupsForVM(name string) []map[string]interface{} {
	bkup := system.LXDBackupPrefix + name
	out := []map[string]interface{}{}

	// Workload-layout backups (canonical from v6.5.19+).
	if workload, err := system.ListWorkloadBackupInstances(); err == nil {
		for _, w := range workload {
			if w.Name != bkup {
				continue
			}
			snaps := system.ListWorkloadBackupSnapshots(w.Dataset)
			snapList := make([]map[string]interface{}, 0, len(snaps))
			for _, s := range snaps {
				snapList = append(snapList, map[string]interface{}{
					"name":       s.Name,
					"created_at": s.CreatedAt,
					"used":       s.Used,
					"written":    s.Written,
				})
			}
			out = append(out, map[string]interface{}{
				"datastore":       w.ZFSPool,
				"backup_instance": bkup,
				"used_bytes":      w.UsedBytes,
				"snapshots":       snapList,
			})
		}
	}

	// Legacy Incus-pool layout (kept for pre-v6.5.19 backups).
	inst, _ := system.LXDListAllBackupInstances()
	for _, i := range inst {
		if i.Name != bkup {
			continue
		}
		snaps := system.LXDListBackupSnapshots(bkup, i.RootPool)
		snapList := make([]map[string]interface{}, 0, len(snaps))
		for _, s := range snaps {
			snapList = append(snapList, map[string]interface{}{
				"name":       s.Name,
				"created_at": s.CreatedAt,
				"used":       s.Used,
				"written":    s.Written,
			})
		}
		out = append(out, map[string]interface{}{
			"datastore":       i.RootPool,
			"backup_instance": bkup,
			"snapshots":       snapList,
		})
	}
	return out
}

// HandleGetLXDBackupPolicy returns the policy for the instance or the empty
// shape if none is saved yet.
// GET /api/incus/instances/{name}/backup-schedule
func HandleGetLXDBackupPolicy(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := mux.Vars(r)["name"]
		backupSchedMu.Lock()
		defer backupSchedMu.Unlock()
		for _, p := range appCfg.LXDBackupPolicies {
			if p.Instance == name {
				jsonOK(w, map[string]interface{}{
					"exists":   true,
					"policy":   p,
					"next_run": lxdBackupNextRun(p, time.Now()),
				})
				return
			}
		}
		jsonOK(w, map[string]interface{}{
			"exists": false,
			"policy": config.LXDBackupPolicy{
				Instance:       name,
				EveryN:         1,
				Unit:           "hour",
				RetentionKind:  "count",
				RetentionCount: 14,
			},
		})
	}
}

// HandlePutLXDBackupPolicy saves (or replaces) the backup policy for an instance.
// PUT /api/incus/instances/{name}/backup-schedule
func HandlePutLXDBackupPolicy(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := mux.Vars(r)["name"]
		sess := MustSession(r)
		var p config.LXDBackupPolicy
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		p.Instance = name
		if p.DestKind != "local" && p.DestKind != "remote" {
			jsonErr(w, http.StatusBadRequest, "dest_kind must be 'local' or 'remote'")
			return
		}
		if p.DestPool == "" {
			jsonErr(w, http.StatusBadRequest, "dest_pool is required")
			return
		}
		if p.DestKind == "remote" {
			if p.DestServerID == "" {
				jsonErr(w, http.StatusBadRequest, "dest_server_id is required for remote destinations")
				return
			}
			if !findLinkedServer(appCfg, p.DestServerID) {
				jsonErr(w, http.StatusBadRequest, "unknown linked server")
				return
			}
		}
		if err := validateBackupInterval(p.Unit, p.EveryN); err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		switch p.RetentionKind {
		case "count":
			if p.RetentionCount < 1 {
				p.RetentionCount = 1
			}
		case "age":
			switch p.RetentionAgeU {
			case "hours", "days", "weeks", "months":
			default:
				jsonErr(w, http.StatusBadRequest, "retention_age_unit must be hours|days|weeks|months")
				return
			}
			if p.RetentionAgeN < 1 {
				p.RetentionAgeN = 1
			}
		default:
			jsonErr(w, http.StatusBadRequest, "retention_kind must be 'count' or 'age'")
			return
		}
		// Compression: empty = default (zstd-19, max ratio).
		if p.Compression == "" {
			p.Compression = "zstd-19"
		}
		if !system.ValidBackupCompressions[p.Compression] {
			jsonErr(w, http.StatusBadRequest, "compression must be zstd-19|zstd-9|zstd-3|lz4|off")
			return
		}
		if p.HourOfDay < 0 || p.HourOfDay > 23 {
			p.HourOfDay = 0
		}
		if p.MinuteOfHour < 0 || p.MinuteOfHour > 59 {
			p.MinuteOfHour = 0
		}
		if p.Weekday < 0 || p.Weekday > 6 {
			p.Weekday = 0
		}
		if p.DayOfMonth < 1 || p.DayOfMonth > 31 {
			p.DayOfMonth = 1
		}

		backupSchedMu.Lock()
		replaced := false
		for i := range appCfg.LXDBackupPolicies {
			if appCfg.LXDBackupPolicies[i].Instance == name {
				p.LastRun = appCfg.LXDBackupPolicies[i].LastRun
				appCfg.LXDBackupPolicies[i] = p
				replaced = true
				break
			}
		}
		if !replaced {
			appCfg.LXDBackupPolicies = append(appCfg.LXDBackupPolicies, p)
		}
		err := config.SaveAppConfig(appCfg)
		backupSchedMu.Unlock()
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		audit.Log(audit.Entry{
			User: sess.Username, Role: sess.Role,
			Action: audit.ActionLXDBackupSchedule,
			Target: name,
			Result: audit.ResultOK,
			Details: fmt.Sprintf("%s:%s every %d %s (%s)", p.DestKind, p.DestPool, p.EveryN, p.Unit,
				map[bool]string{true: "enabled", false: "disabled"}[p.Enabled]),
		})
		jsonOK(w, map[string]interface{}{"ok": true, "next_run": lxdBackupNextRun(p, time.Now())})
	}
}

// HandleDeleteLXDBackupPolicy removes the saved policy for an instance.
// DELETE /api/incus/instances/{name}/backup-schedule
func HandleDeleteLXDBackupPolicy(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := mux.Vars(r)["name"]
		sess := MustSession(r)
		backupSchedMu.Lock()
		kept := appCfg.LXDBackupPolicies[:0]
		removed := false
		for _, p := range appCfg.LXDBackupPolicies {
			if p.Instance == name {
				removed = true
				continue
			}
			kept = append(kept, p)
		}
		appCfg.LXDBackupPolicies = kept
		err := config.SaveAppConfig(appCfg)
		backupSchedMu.Unlock()
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if removed {
			audit.Log(audit.Entry{
				User: sess.Username, Role: sess.Role,
				Action:  audit.ActionLXDBackupSchedule,
				Target:  name,
				Result:  audit.ResultOK,
				Details: "policy removed",
			})
		}
		jsonOK(w, map[string]bool{"ok": true})
	}
}

func validateBackupInterval(unit string, n int) error {
	allowed, ok := allowedBackupUnits[unit]
	if !ok {
		return fmt.Errorf("unit must be minute|hour|day|week|month")
	}
	for _, v := range allowed {
		if v == n {
			return nil
		}
	}
	return fmt.Errorf("invalid every_n %d for unit %s (minimum effective period is 20 minutes)", n, unit)
}

// HandleLXDBackupNow starts an immediate backup using the saved policy (or
// the body's policy if "ad_hoc" is true).
// POST /api/incus/instances/{name}/backup-now
//
// Body (all optional): {"dest_kind","dest_server_id","dest_pool"} — when set,
// runs an ad-hoc backup with those parameters; otherwise uses the saved policy.
func HandleLXDBackupNow(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := mux.Vars(r)["name"]
		sess := MustSession(r)
		var req struct {
			DestKind     string `json:"dest_kind"`
			DestServerID string `json:"dest_server_id"`
			DestPool     string `json:"dest_pool"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)

		// Resolve policy.
		var policy config.LXDBackupPolicy
		if req.DestKind != "" {
			policy = config.LXDBackupPolicy{
				Instance:     name,
				DestKind:     req.DestKind,
				DestServerID: req.DestServerID,
				DestPool:     req.DestPool,
			}
		} else {
			backupSchedMu.Lock()
			found := false
			for _, p := range appCfg.LXDBackupPolicies {
				if p.Instance == name {
					policy = p
					found = true
					break
				}
			}
			backupSchedMu.Unlock()
			if !found {
				jsonErr(w, http.StatusBadRequest, "no backup destination — pass dest_kind/dest_pool or save a schedule first")
				return
			}
		}

		jobID, err := startBackupJob(appCfg, policy, sess.Username, sess.Role)
		if err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"job_id": jobID})
	}
}

// HandleLXDBackupProgress returns the running log + status of a backup job.
// GET /api/incus/backup-jobs/{job_id}/progress
func HandleLXDBackupProgress(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["job_id"]
	v, ok := lxdBackupJobs.Load(id)
	if !ok {
		jsonErr(w, http.StatusNotFound, "job not found")
		return
	}
	j := v.(*lxdBackupJob)
	j.mu.Lock()
	defer j.mu.Unlock()
	jsonOK(w, map[string]interface{}{
		"status":     j.Status,
		"error":      j.Error,
		"lines":      j.Lines,
		"instance":   j.Instance,
		"dest_kind":  j.DestKind,
		"dest_host":  j.DestHost,
		"dest_pool":  j.DestPool,
		"started_at": j.StartedAt,
	})
}

// HandleInstallSyncoid runs apt-get install -y sanoid.
// POST /api/prerequisites/install-syncoid
func HandleInstallSyncoid(w http.ResponseWriter, r *http.Request) {
	out, err := system.InstallSyncoid()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, strings.TrimSpace(string(out)))
		return
	}
	jsonOK(w, map[string]string{"message": "syncoid installed"})
}

// HandleLXDBackupCancel cancels a running job.
// POST /api/incus/backup-jobs/{job_id}/cancel
func HandleLXDBackupCancel(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["job_id"]
	v, ok := lxdBackupJobs.Load(id)
	if !ok {
		jsonErr(w, http.StatusNotFound, "job not found")
		return
	}
	v.(*lxdBackupJob).cancel()
	jsonOK(w, map[string]bool{"ok": true})
}

// --- Scheduler ---

// StartLXDBackupScheduler launches the per-minute backup ticker.
func StartLXDBackupScheduler(appCfg *config.AppConfig) {
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for now := range t.C {
			tickLXDBackupPolicies(now, appCfg)
		}
	}()
}

func tickLXDBackupPolicies(now time.Time, appCfg *config.AppConfig) {
	backupSchedMu.Lock()
	policies := make([]config.LXDBackupPolicy, len(appCfg.LXDBackupPolicies))
	copy(policies, appCfg.LXDBackupPolicies)
	backupSchedMu.Unlock()

	for _, p := range policies {
		if !p.Enabled || !lxdBackupDue(p, now) {
			continue
		}
		_, err := startBackupJob(appCfg, p, "system", "system")
		if err != nil {
			log.Printf("[lxd-backup-sched] %s: %v", p.Instance, err)
		}
	}
}

func lxdBackupDue(p config.LXDBackupPolicy, now time.Time) bool {
	if p.EveryN <= 0 || p.Unit == "" {
		return false
	}
	switch p.Unit {
	case "minute":
		return now.Minute()%p.EveryN == 0
	case "hour":
		return now.Minute() == p.MinuteOfHour && now.Hour()%p.EveryN == 0
	case "day":
		if now.Hour() != p.HourOfDay || now.Minute() != p.MinuteOfHour {
			return false
		}
		days := now.Unix() / 86400
		return days%int64(p.EveryN) == 0
	case "week":
		// v6.5.19+: weekday is configurable. Shares the helper with
		// the snapshot scheduler so both paths normalize identically.
		if int(now.Weekday()) != normalizeWeekday(p.Weekday) {
			return false
		}
		if now.Hour() != p.HourOfDay || now.Minute() != p.MinuteOfHour {
			return false
		}
		weeks := now.Unix() / (86400 * 7)
		return weeks%int64(p.EveryN) == 0
	case "month":
		want := effectiveDayOfMonth(p.DayOfMonth, now)
		if now.Day() != want || now.Hour() != p.HourOfDay || now.Minute() != p.MinuteOfHour {
			return false
		}
		return int(now.Month())%p.EveryN == 1
	}
	return false
}

func lxdBackupNextRun(p config.LXDBackupPolicy, from time.Time) time.Time {
	t := from.Truncate(time.Minute).Add(time.Minute)
	end := from.Add(31 * 24 * time.Hour)
	for ; t.Before(end); t = t.Add(time.Minute) {
		if lxdBackupDue(p, t) {
			return t
		}
	}
	return time.Time{}
}

// startBackupJob registers a new job, launches the goroutine, and returns
// the job id immediately. Caller handles audit + HTTP response.
func startBackupJob(appCfg *config.AppConfig, p config.LXDBackupPolicy, user, role string) (string, error) {
	if !system.SyncoidPrereqsInstalled() {
		return "", fmt.Errorf("syncoid is not installed (Prerequisites → ZFS Replication)")
	}
	if _, err := system.LXDGetStatus(p.Instance); err != nil {
		return "", fmt.Errorf("source instance not found: %w", err)
	}

	id := fmt.Sprintf("bkj-%d", time.Now().UnixNano())
	ctx, cancel := context.WithCancel(context.Background())
	job := &lxdBackupJob{
		Status:    "queued",
		Instance:  p.Instance,
		DestKind:  p.DestKind,
		DestPool:  p.DestPool,
		StartedAt: time.Now(),
		cancelFn:  cancel,
	}
	lxdBackupJobs.Store(id, job)

	go func() {
		err := runBackupJob(ctx, job, p, appCfg)

		job.mu.Lock()
		if err != nil && err != context.Canceled && job.Status != "canceled" {
			job.Status = "error"
			job.Error = err.Error()
		} else if job.Status != "canceled" {
			job.Status = "done"
		}
		job.mu.Unlock()

		// Persist last-status on the saved policy.
		updateBackupPolicyStatus(appCfg, p.Instance, err)

		// Audit.
		result := audit.ResultOK
		details := ""
		if err != nil && err != context.Canceled {
			result = audit.ResultError
			details = err.Error()
		}
		audit.Log(audit.Entry{
			User:    user,
			Role:    role,
			Action:  audit.ActionLXDBackup,
			Target:  fmt.Sprintf("%s → %s:%s", p.Instance, p.DestKind, p.DestPool),
			Result:  result,
			Details: details,
		})
	}()
	return id, nil
}

func runBackupJob(ctx context.Context, job *lxdBackupJob, p config.LXDBackupPolicy, appCfg *config.AppConfig) error {
	job.mu.Lock()
	job.Status = "running"
	job.mu.Unlock()

	logFn := job.appendLine

	now := time.Now()

	// v6.5.19+: source-side snapshot is named after its destination so the
	// user can tell at a glance which backup target each snapshot belongs
	// to. Format:
	//   bkp-to-<label>-YYYY-MM-DD-HHMMSS
	// where <label> is "local-<datastore>" for local backups or the
	// peer's hostname for remote. After a successful fire we prune older
	// source snapshots with the same prefix so only the most recent
	// anchor lingers — see retention logic at the end of this function.
	destLabel := "local-" + p.DestPool
	if p.DestKind == "remote" {
		if ls := getLinkedServer(appCfg, p.DestServerID); ls != nil && ls.Hostname != "" {
			destLabel = ls.Hostname
		} else {
			destLabel = "remote-" + p.DestPool
		}
	}
	snapPrefix := system.LXDBackupSnapshotPrefixFor(destLabel)
	snapName := system.LXDSnapshotName(snapPrefix, now)

	logFn(fmt.Sprintf("Creating source snapshot %s/%s", p.Instance, snapName))
	if err := system.CreateLXDSnapshot(p.Instance, snapName, false); err != nil {
		return fmt.Errorf("create snapshot: %w", err)
	}

	// Enumerate every dataset that belongs to the instance — root fs, VM
	// .block zvol, and any attached custom-volume disks. This is the
	// critical bit for VM correctness: --recursive on the root fs alone
	// does NOT include the sibling .block zvol.
	parts, err := system.LXDInstanceBackupDatasets(p.Instance)
	if err != nil {
		return fmt.Errorf("enumerate disks: %w", err)
	}
	if len(parts) == 0 {
		return fmt.Errorf("no datasets found to back up for %s", p.Instance)
	}
	kind, err := system.LXDInstanceKind(p.Instance)
	if err != nil {
		return err
	}
	logFn(fmt.Sprintf("Found %d dataset(s) to replicate for %s", len(parts), p.Instance))

	if p.DestKind == "local" {
		// v6.5.19+: local destinations use the workload layout too.
		// DestPool is the ZFS pool name (e.g. "BIGRAID5"), NOT an Incus
		// storage pool name. EnsureWorkloadParent creates
		// "<pool>/ZNAS-Backups-Workload" + "/<kind>" with the chosen
		// compression. Backups live there independent of Incus.
		if err := system.EnsureWorkloadParent(p.DestPool, kind, p.Compression); err != nil {
			return fmt.Errorf("prep workload parent: %w", err)
		}
		if err := system.EnsureWorkloadParent(p.DestPool, "custom", p.Compression); err != nil {
			logFn("prep custom workload parent: " + err.Error())
		}
		workloadParent := system.LXDWorkloadBackupParent(p.DestPool)
		for _, part := range parts {
			parent := workloadParent + "/" + kind
			if part.Kind == "custom" {
				parent = workloadParent + "/custom"
			}
			dstDataset := parent + "/" + part.DstBaseName
			// Self-heal a broken incremental chain. If the user deleted
			// source snapshots from the Snapshots UI and no shared
			// snapshot remains, wipe the destination so syncoid does a
			// clean full send instead of erroring out with "no common
			// ancestor". The log line announces the wipe so the user
			// knows why the next fire took longer.
			system.PrepLocalBackupChain(part.SrcDataset, dstDataset, logFn)
			logFn(fmt.Sprintf("[%s] %s -> %s (compression=%s)", part.Kind, part.SrcDataset, dstDataset, p.Compression))
			if err := system.RunSyncoidLocal(ctx, part.SrcDataset, dstDataset, part.Recursive, logFn); err != nil {
				return err
			}
			applyBackupRetention(dstDataset, p, logFn)
		}
		// Prune older source anchors with the same destination prefix.
		// The latest one — just taken — stays as the anchor for the
		// next incremental. Older ones aren't needed and clutter the
		// VM's Snapshots dropdown.
		if pruned, _ := system.PruneSourceBackupAnchors(p.Instance, snapPrefix); len(pruned) > 0 {
			logFn(fmt.Sprintf("Pruned %d older source anchor(s) (%s-*): %s", len(pruned), snapPrefix, strings.Join(pruned, ", ")))
		}
		logFn(fmt.Sprintf("Backup of %s saved under %s/ZNAS-Backups-Workload — visible in the Backups page.", p.Instance, p.DestPool))
		return nil
	}

	// Remote
	ls := getLinkedServer(appCfg, p.DestServerID)
	if ls == nil {
		return fmt.Errorf("linked server %q not found", p.DestServerID)
	}
	pubKey, err := system.EnsureSSHKey()
	if err != nil {
		return fmt.Errorf("ssh key: %w", err)
	}
	// Pushing the SSH key is idempotent and only needs to land ONCE per
	// peer — every prior successful backup to this peer already did it.
	// So a failure here (typically a momentarily-overloaded peer timing
	// out the HMAC call) is NOT fatal: log it and continue. If the key
	// genuinely isn't on the peer, syncoid fails later with a clear
	// "Permission denied (publickey)" that the user can act on.
	if err := system.SendPushSSHKey(ls.URL, ls.SharedSecret, pubKey, ls.TLSFingerprint); err != nil {
		logFn("warning: could not refresh SSH key on peer (" + err.Error() + ") — continuing; the key from a previous backup should still be in place.")
	}
	remoteURL, err := url.Parse(ls.URL)
	if err != nil {
		return fmt.Errorf("invalid linked server URL: %w", err)
	}
	remoteInfo, err := system.GetRemotePools(ls.URL, ls.SharedSecret, ls.TLSFingerprint)
	if err != nil || remoteInfo == nil || remoteInfo.ProcessUser == "" {
		return fmt.Errorf("cannot reach remote server")
	}
	if err := system.EnsureRemoteZFSAccess(ls.URL, ls.SharedSecret, ls.TLSFingerprint); err != nil {
		return fmt.Errorf("grant zfs on remote: %w", err)
	}
	// Resolve a usable SSH transport. The InterLink URL host may be a
	// reverse proxy that forwards only HTTPS — syncoid needs a direct
	// SSH route. The peer advertises its real IPs in SSHHosts; probe
	// each (plus the URL hostname as fallback) and use the first that
	// authenticates.
	remoteHost := system.PickReachableSSHHost(remoteInfo.SSHHosts, remoteURL.Hostname(), remoteInfo.ProcessUser)
	if remoteHost == "" {
		return fmt.Errorf("no SSH-reachable address for peer %s — tried %v and %s. "+
			"The InterLink URL may point at a reverse proxy; ensure the peer's LAN IP is reachable and its zfsnas SSH key is trusted.",
			ls.Hostname, remoteInfo.SSHHosts, remoteURL.Hostname())
	}
	logFn("SSH transport to peer: " + remoteHost)

	// v6.5.19+: remote destinations don't need Incus on the peer. The
	// backup lands in <peer-zfs-pool>/ZNAS-Backups-Workload/<kind>/bkup--<vm>
	// (plus the .block sibling and any custom volumes). `p.DestPool` is
	// the ZFS pool NAME on the peer (from the peer's `zpool list`), not
	// an Incus storage-pool name.
	//
	// Ask the peer to ensure the workload parent exists with the user's
	// chosen compression algorithm BEFORE syncoid writes any data, so
	// the first stream is compressed correctly.
	if err := system.InterlinkRemotePrepWorkload(ls.URL, ls.SharedSecret, ls.TLSFingerprint, p.DestPool, kind, p.Compression); err != nil {
		return fmt.Errorf("prep remote workload parent: %w", err)
	}
	_ = system.InterlinkRemotePrepWorkload(ls.URL, ls.SharedSecret, ls.TLSFingerprint, p.DestPool, "custom", p.Compression)
	workloadParent := system.LXDWorkloadBackupParent(p.DestPool)

	job.mu.Lock()
	job.DestHost = ls.Hostname
	job.mu.Unlock()

	// Self-heal a broken chain on the remote. Send the peer the snapshot
	// list of the SOURCE root-fs dataset (and .block sibling, since they
	// share names from `incus snapshot create`). If the peer's destination
	// has no overlap, it destroys the destination so the upcoming syncoid
	// pull does a clean full send instead of erroring with "no common
	// ancestor".
	srcSnaps := []string{}
	for n := range system.SnapshotNameSetForBackups(parts[0].SrcDataset) {
		srcSnaps = append(srcSnaps, n)
	}
	if chainResp, perr := system.InterlinkRemotePrepChain(ls.URL, ls.SharedSecret, ls.TLSFingerprint, p.DestPool, p.Instance, srcSnaps); perr == nil && chainResp != nil && chainResp.Wiped {
		logFn("Remote backup chain was broken (no shared snapshot) — peer wiped destination datasets, this fire will do a full re-send.")
		for _, w := range chainResp.WipedPaths {
			logFn("  wiped on peer: " + w)
		}
	}

	for _, part := range parts {
		parent := workloadParent + "/" + kind
		if part.Kind == "custom" {
			parent = workloadParent + "/custom"
		}
		dstDataset := parent + "/" + part.DstBaseName
		logFn(fmt.Sprintf("[%s] %s -> %s@%s:%s", part.Kind, part.SrcDataset, remoteInfo.ProcessUser, remoteHost, dstDataset))
		if err := system.RunSyncoidRemote(ctx, part.SrcDataset, remoteHost, remoteInfo.ProcessUser, dstDataset, part.Recursive, logFn); err != nil {
			return err
		}
	}
	// Prune older source anchors with the same destination prefix. The
	// just-taken one stays as the next incremental's anchor.
	if pruned, _ := system.PruneSourceBackupAnchors(p.Instance, snapPrefix); len(pruned) > 0 {
		logFn(fmt.Sprintf("Pruned %d older source anchor(s) (%s-*): %s", len(pruned), snapPrefix, strings.Join(pruned, ", ")))
	}
	// Retention on the remote runs in a future iteration; the dataset(s)
	// land successfully which is what matters for disaster recovery today.
	return nil
}

func applyBackupRetention(dataset string, p config.LXDBackupPolicy, logFn func(string)) {
	if p.RetentionKind == "count" && p.RetentionCount > 0 {
		if err := system.LXDPruneRetentionByCount(dataset, "auto", p.RetentionCount); err != nil {
			logFn("retention prune: " + err.Error())
		}
		return
	}
	if p.RetentionKind == "age" && p.RetentionAgeN > 0 {
		var cutoff time.Time
		switch p.RetentionAgeU {
		case "hours":
			cutoff = time.Now().Add(-time.Duration(p.RetentionAgeN) * time.Hour)
		case "days":
			cutoff = time.Now().Add(-time.Duration(p.RetentionAgeN) * 24 * time.Hour)
		case "weeks":
			cutoff = time.Now().Add(-time.Duration(p.RetentionAgeN) * 7 * 24 * time.Hour)
		case "months":
			cutoff = time.Now().AddDate(0, -p.RetentionAgeN, 0)
		default:
			return
		}
		if err := system.LXDPruneRetentionByAge(dataset, "auto", cutoff); err != nil {
			logFn("retention prune: " + err.Error())
		}
	}
}

func updateBackupPolicyStatus(appCfg *config.AppConfig, instance string, err error) {
	backupSchedMu.Lock()
	defer backupSchedMu.Unlock()
	for i := range appCfg.LXDBackupPolicies {
		if appCfg.LXDBackupPolicies[i].Instance != instance {
			continue
		}
		appCfg.LXDBackupPolicies[i].LastRun = time.Now()
		if err != nil {
			appCfg.LXDBackupPolicies[i].LastStatus = "error"
			appCfg.LXDBackupPolicies[i].LastError = err.Error()
		} else {
			appCfg.LXDBackupPolicies[i].LastStatus = "ok"
			appCfg.LXDBackupPolicies[i].LastError = ""
		}
		_ = config.SaveAppConfig(appCfg)
		return
	}
}

func getLinkedServer(appCfg *config.AppConfig, id string) *config.LinkedServer {
	for i := range appCfg.InterLink {
		if appCfg.InterLink[i].ID == id {
			return &appCfg.InterLink[i]
		}
	}
	return nil
}

func findLinkedServer(appCfg *config.AppConfig, id string) bool {
	return getLinkedServer(appCfg, id) != nil
}

// lookupRemoteDatastoreSource — DEPRECATED v6.5.19+; remote backups use
// the workload layout (LXDWorkloadBackupParent) directly on the peer's
// ZFS pool, not an Incus storage source. Kept for now in case any older
// code path still calls it.
//
// nolint:unused
// lookupRemoteDatastoreSource_legacy queries a peer for the ZFS source of one of
// its Incus storage pools. Reuses the existing HMAC plumbing.
func lookupRemoteDatastoreSource(ls *config.LinkedServer, poolName string) (string, error) {
	// We piggyback on the existing remote-lxd-storage HMAC endpoint by
	// asking for the pool list, then fetching the specific pool source via
	// a new GET /api/lxd/interlink-pool-source?name=<pool>.
	src, err := system.InterlinkRemotePoolSource(ls.URL, ls.SharedSecret, ls.TLSFingerprint, poolName)
	if err != nil {
		return "", err
	}
	if src == "" {
		return "", fmt.Errorf("remote pool %q has no zfs source", poolName)
	}
	return src, nil
}
