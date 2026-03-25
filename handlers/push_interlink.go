package handlers

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"strings"
	"zfsnas/internal/audit"
	"zfsnas/internal/config"
	"zfsnas/internal/pushinterlink"
	"zfsnas/system"

	"github.com/gorilla/mux"
)

// HandlePushInterlinkGetRemotePools handles GET /api/interlink/remote-pools/{server_id}.
// Proxies an HMAC-auth call to the linked remote server and returns its pool list.
func HandlePushInterlinkGetRemotePools(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := mux.Vars(r)["server_id"]
		var ls *config.LinkedServer
		for i := range appCfg.InterLink {
			if appCfg.InterLink[i].ID == id {
				ls = &appCfg.InterLink[i]
				break
			}
		}
		if ls == nil {
			jsonErr(w, http.StatusNotFound, "linked server not found")
			return
		}
		resp, err := system.GetRemotePools(ls.URL, ls.SharedSecret)
		if err != nil {
			jsonErr(w, http.StatusBadGateway, "cannot get remote pools: "+err.Error())
			return
		}
		jsonOK(w, resp)
	}
}

// HandlePushInterlinkStart handles POST /api/push-interlink/start (admin only).
// Sets up SSH key exchange then launches a background zfs send | ssh zfs recv job.
func HandlePushInterlinkStart(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := MustSession(r)
		var req struct {
			SnapshotName string `json:"snapshot_name"`
			ServerID     string `json:"server_id"`
			DestDataset  string `json:"dest_dataset"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if req.SnapshotName == "" || req.ServerID == "" || req.DestDataset == "" {
			jsonErr(w, http.StatusBadRequest, "snapshot_name, server_id and dest_dataset are required")
			return
		}

		var ls *config.LinkedServer
		for i := range appCfg.InterLink {
			if appCfg.InterLink[i].ID == req.ServerID {
				ls = &appCfg.InterLink[i]
				break
			}
		}
		if ls == nil {
			jsonErr(w, http.StatusNotFound, "linked server not found")
			return
		}

		// Ensure our SSH key exists and register it on the remote.
		pubKey, err := system.EnsureSSHKey()
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, "cannot set up SSH key: "+err.Error())
			return
		}
		if err := system.SendPushSSHKey(ls.URL, ls.SharedSecret, pubKey); err != nil {
			jsonErr(w, http.StatusBadGateway, "cannot push SSH key to remote: "+err.Error())
			return
		}

		// Parse host from the linked server URL.
		remoteURL, err := url.Parse(ls.URL)
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, "invalid linked server URL")
			return
		}
		remoteHost := remoteURL.Hostname()

		// Get the remote process user.
		remoteInfo, err := system.GetRemotePools(ls.URL, ls.SharedSecret)
		if err != nil {
			jsonErr(w, http.StatusBadGateway, "cannot reach remote server: "+err.Error())
			return
		}
		if remoteInfo.ProcessUser == "" {
			jsonErr(w, http.StatusBadGateway, "remote server did not return process user")
			return
		}
		remoteUser := remoteInfo.ProcessUser

		// Ensure ZFS permissions are set on both sides before we start.
		// This covers pools created after the InterLink was first established.
		if err := system.GrantLocalZFSAccess(); err != nil {
			log.Printf("push-interlink: local zfs allow: %v", err)
		}
		if err := system.EnsureRemoteZFSAccess(ls.URL, ls.SharedSecret); err != nil {
			jsonErr(w, http.StatusBadGateway, "cannot grant ZFS permissions on remote: "+err.Error())
			return
		}

		// Capture values for the goroutine closure.
		snapshot := req.SnapshotName
		destDataset := req.DestDataset
		username := sess.Username
		role := sess.Role
		hostname := ls.Hostname

		ctx, cancel := context.WithCancel(context.Background())
		job := pushinterlink.Default.Register(snapshot, destDataset, hostname, username, cancel)

		go func() {
			totalBytes := system.GetSnapshotSize(snapshot)
			pushinterlink.Default.SetRunning(job.ID, totalBytes)

			jobErr := system.RunZFSPush(ctx, snapshot, remoteHost, remoteUser, destDataset, totalBytes,
				func(sent int64) {
					pushinterlink.Default.UpdateProgress(job.ID, sent)
				},
			)
			pushinterlink.Default.Finish(job.ID, jobErr)

			result, details := audit.ResultOK, ""
			if jobErr != nil && jobErr != context.Canceled {
				result = audit.ResultError
				details = jobErr.Error()
			}
			audit.Log(audit.Entry{
				User:    username,
				Role:    role,
				Action:  audit.ActionPushInterlink,
				Target:  snapshot,
				Result:  result,
				Details: details,
			})
		}()

		jsonOK(w, map[string]interface{}{
			"job_id": job.ID,
			"ok":     true,
		})
	}
}

// HandlePushInterlinkStartDataset handles POST /api/push-interlink/start-dataset (admin only).
// Creates a temporary snapshot of the local dataset or zvol, pushes it to the remote
// InterLink server over SSH, then destroys the temporary snapshot on completion.
func HandlePushInterlinkStartDataset(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess := MustSession(r)
		var req struct {
			LocalDataset string `json:"local_dataset"`
			ServerID     string `json:"server_id"`
			DestDataset  string `json:"dest_dataset"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if req.LocalDataset == "" || req.ServerID == "" || req.DestDataset == "" {
			jsonErr(w, http.StatusBadRequest, "local_dataset, server_id and dest_dataset are required")
			return
		}

		var ls *config.LinkedServer
		for i := range appCfg.InterLink {
			if appCfg.InterLink[i].ID == req.ServerID {
				ls = &appCfg.InterLink[i]
				break
			}
		}
		if ls == nil {
			jsonErr(w, http.StatusNotFound, "linked server not found")
			return
		}

		// Create the temporary snapshot before launching the background job.
		tempSnap, err := system.CreateSnapshot(req.LocalDataset, "znas-push")
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, "cannot create snapshot: "+err.Error())
			return
		}

		// Ensure our SSH key exists and register it on the remote.
		pubKey, err := system.EnsureSSHKey()
		if err != nil {
			system.DestroySnapshot(tempSnap) //nolint:errcheck
			jsonErr(w, http.StatusInternalServerError, "cannot set up SSH key: "+err.Error())
			return
		}
		if err := system.SendPushSSHKey(ls.URL, ls.SharedSecret, pubKey); err != nil {
			system.DestroySnapshot(tempSnap) //nolint:errcheck
			jsonErr(w, http.StatusBadGateway, "cannot push SSH key to remote: "+err.Error())
			return
		}

		remoteURL, err := url.Parse(ls.URL)
		if err != nil {
			system.DestroySnapshot(tempSnap) //nolint:errcheck
			jsonErr(w, http.StatusInternalServerError, "invalid linked server URL")
			return
		}
		remoteHost := remoteURL.Hostname()

		remoteInfo, err := system.GetRemotePools(ls.URL, ls.SharedSecret)
		if err != nil {
			system.DestroySnapshot(tempSnap) //nolint:errcheck
			jsonErr(w, http.StatusBadGateway, "cannot reach remote server: "+err.Error())
			return
		}
		if remoteInfo.ProcessUser == "" {
			system.DestroySnapshot(tempSnap) //nolint:errcheck
			jsonErr(w, http.StatusBadGateway, "remote server did not return process user")
			return
		}

		// Ensure ZFS permissions are set on both sides before we start.
		// This covers pools created after the InterLink was first established.
		if err := system.GrantLocalZFSAccess(); err != nil {
			system.DestroySnapshot(tempSnap) //nolint:errcheck
			log.Printf("push-interlink: local zfs allow: %v", err)
		}
		if err := system.EnsureRemoteZFSAccess(ls.URL, ls.SharedSecret); err != nil {
			system.DestroySnapshot(tempSnap) //nolint:errcheck
			jsonErr(w, http.StatusBadGateway, "cannot grant ZFS permissions on remote: "+err.Error())
			return
		}

		localDataset := req.LocalDataset
		destDataset := req.DestDataset
		username := sess.Username
		role := sess.Role
		hostname := ls.Hostname
		remoteUser := remoteInfo.ProcessUser

		ctx, cancel := context.WithCancel(context.Background())
		// Use the local dataset name as the snapshot_name for display in the activity bar.
		job := pushinterlink.Default.Register(localDataset, destDataset, hostname, username, cancel)
		// Record the temp snapshot so ForceCancel can clean it up if the goroutine gets stuck.
		pushinterlink.Default.SetTempSnapshot(job.ID, tempSnap)

		go func() {
			// Always clean up the temporary snapshot when the job ends.
			defer system.DestroySnapshot(tempSnap) //nolint:errcheck

			totalBytes := system.GetSnapshotSize(tempSnap)
			pushinterlink.Default.SetRunning(job.ID, totalBytes)

			jobErr := system.RunZFSPush(ctx, tempSnap, remoteHost, remoteUser, destDataset, totalBytes,
				func(sent int64) {
					pushinterlink.Default.UpdateProgress(job.ID, sent)
				},
			)

			// On success, clean up the landing snapshot on the remote.
			// The snapshot name is preserved by zfs recv: destDataset@<same-suffix>.
			if jobErr == nil {
				if idx := strings.Index(tempSnap, "@"); idx >= 0 {
					remoteSnap := destDataset + tempSnap[idx:]
					system.DestroyRemoteSnapshot(context.Background(), remoteSnap, remoteHost, remoteUser) //nolint:errcheck
				}
			}

			pushinterlink.Default.Finish(job.ID, jobErr)

			result, details := audit.ResultOK, ""
			if jobErr != nil && jobErr != context.Canceled {
				result = audit.ResultError
				details = jobErr.Error()
			}
			audit.Log(audit.Entry{
				User:    username,
				Role:    role,
				Action:  audit.ActionPushInterlink,
				Target:  localDataset,
				Result:  result,
				Details: details,
			})
		}()

		jsonOK(w, map[string]interface{}{
			"job_id": job.ID,
			"ok":     true,
		})
	}
}

// HandlePushInterlinkJobs handles GET /api/push-interlink/jobs.
func HandlePushInterlinkJobs(w http.ResponseWriter, r *http.Request) {
	jobs := pushinterlink.Default.List()
	jsonOK(w, jobs)
}

// HandlePushInterlinkCancel handles POST /api/push-interlink/cancel/{id} (admin only).
// Uses ForceCancel so the job is immediately removed from the active list even if the
// underlying SSH process is stuck in an uninterruptible kernel wait (D-state).
// Any temporary snapshot recorded on the job is destroyed asynchronously.
func HandlePushInterlinkCancel(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	snap, ok := pushinterlink.Default.ForceCancel(id)
	if !ok {
		jsonErr(w, http.StatusNotFound, "job not found or already finished")
		return
	}
	if snap != "" {
		go system.DestroySnapshot(snap) //nolint:errcheck
	}
	jsonOK(w, map[string]bool{"ok": true})
}
