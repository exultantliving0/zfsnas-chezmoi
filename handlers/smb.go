package handlers

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"zfsnas/internal/alerts"
	"zfsnas/internal/audit"
	"zfsnas/internal/config"
	"zfsnas/system"

	"github.com/gorilla/mux"
)

// HandleGetSMBGlobalConfig returns global Samba settings (max processes, home dataset).
func HandleGetSMBGlobalConfig(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonOK(w, map[string]interface{}{
			"max_smbd_processes": appCfg.MaxSmbdProcesses,
			"home_dataset":       appCfg.SMBHomeDataset,
			"clean_defaults":     appCfg.SMBCleanDefaults,
		})
	}
}

// HandleUpdateSMBGlobalConfig saves global Samba settings and applies them immediately.
func HandleUpdateSMBGlobalConfig(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			MaxSmbdProcesses *int    `json:"max_smbd_processes"`
			HomeDataset      *string `json:"home_dataset"`
			CleanDefaults    *bool   `json:"clean_defaults"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}

		changed := false
		homeDatasetChanged := false
		prevHomeDataset := appCfg.SMBHomeDataset
		if req.MaxSmbdProcesses != nil {
			if *req.MaxSmbdProcesses < 1 || *req.MaxSmbdProcesses > 10000 {
				jsonErr(w, http.StatusBadRequest, "max_smbd_processes must be between 1 and 10000")
				return
			}
			appCfg.MaxSmbdProcesses = *req.MaxSmbdProcesses
			changed = true
		}
		if req.HomeDataset != nil {
			ds := strings.TrimSpace(*req.HomeDataset)
			if ds != "" && !system.DatasetExists(ds) {
				jsonErr(w, http.StatusBadRequest, "dataset not found: "+ds)
				return
			}
			if ds != appCfg.SMBHomeDataset {
				appCfg.SMBHomeDataset = ds
				homeDatasetChanged = true
			}
			changed = true
		}
		if req.CleanDefaults != nil {
			appCfg.SMBCleanDefaults = *req.CleanDefaults
			changed = true
		}

		if !changed {
			jsonOK(w, map[string]string{"message": "no changes"})
			return
		}
		if err := config.SaveAppConfig(appCfg); err != nil {
			jsonErr(w, http.StatusInternalServerError, "failed to save settings")
			return
		}
		// When the home dataset changes, create home dirs in the new dataset and
		// remove empty home dirs from the previous dataset.
		if homeDatasetChanged {
			users, _ := config.LoadUsers()
			for _, u := range users {
				if !u.SMBHomeFolder {
					continue
				}
				if appCfg.SMBHomeDataset != "" {
					if err := system.EnsureSMBHomeDir(appCfg.SMBHomeDataset, u.Username); err != nil {
						log.Printf("smb global config: EnsureSMBHomeDir %s: %v", u.Username, err)
					}
				}
				if prevHomeDataset != "" {
					if err := system.RemoveSMBHomeDirIfEmpty(prevHomeDataset, u.Username); err != nil {
						log.Printf("smb global config: RemoveSMBHomeDirIfEmpty %s: %v", u.Username, err)
					}
				}
			}
		}
		if system.IsSambaInstalled() {
			if err := system.ApplySmbGlobal(appCfg.MaxSmbdProcesses, appCfg.SMBHomeDataset, smbHomeUsernames(), appCfg.SMBCleanDefaults); err != nil {
				log.Printf("smb global config: ApplySmbGlobal: %v", err)
			} else if homeDatasetChanged {
				// [homes] section changes require a full restart to take effect.
				if err := system.RestartSamba(); err != nil {
					log.Printf("smb global config: RestartSamba: %v", err)
				}
			} else if err := system.ReloadSamba(); err != nil {
				log.Printf("smb global config: ReloadSamba: %v", err)
			}
		}
		sess := MustSession(r)
		audit.Log(audit.Entry{
			User:    sess.Username,
			Role:    sess.Role,
			Action:  audit.ActionUpdateSettings,
			Result:  audit.ResultOK,
			Details: "SMB global config updated",
		})
		jsonOK(w, map[string]string{"message": "SMB global settings saved"})
	}
}

// smbHomeUsernames returns the usernames of all users with SMBHomeFolder enabled.
func smbHomeUsernames() []string {
	users, err := config.LoadUsers()
	if err != nil {
		return nil
	}
	var names []string
	for _, u := range users {
		if u.SMBHomeFolder {
			names = append(names, u.Username)
		}
	}
	return names
}

// HandleGetSMBSessions returns active Samba connections grouped by share name.
func HandleGetSMBSessions(w http.ResponseWriter, r *http.Request) {
	if !system.IsSambaInstalled() {
		jsonOK(w, map[string][]system.ShareClient{})
		return
	}
	jsonOK(w, system.GetSMBSessions())
}

// HandleSMBStatus returns Samba installation and service status.
func HandleSMBStatus(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, map[string]interface{}{
		"installed": system.IsSambaInstalled(),
		"status":    system.SambaStatus(),
	})
}

// HandleSMBService starts, stops, or restarts the smbd systemd unit.
func HandleSMBService(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := system.ControlSamba(req.Action); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]string{
		"status": system.SambaStatus(),
	})
}

// HandleListShares returns all configured SMB shares.
func HandleListShares(w http.ResponseWriter, r *http.Request) {
	shares, err := system.ListSMBShares(config.Dir())
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, shares)
}

// HandleCreateShare creates a new SMB share.
func HandleCreateShare(w http.ResponseWriter, r *http.Request) {
	var req system.SMBShare
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Path = strings.TrimSpace(req.Path)
	if req.Name == "" {
		jsonErr(w, http.StatusBadRequest, "share name is required")
		return
	}
	if req.Path == "" {
		jsonErr(w, http.StatusBadRequest, "path is required")
		return
	}

	shares, err := system.ListSMBShares(config.Dir())
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, s := range shares {
		if strings.EqualFold(s.Name, req.Name) {
			jsonErr(w, http.StatusConflict, "a share with that name already exists")
			return
		}
	}

	shares = append(shares, req)
	if err := system.SaveSMBShares(config.Dir(), shares); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := system.ReloadSamba(); err != nil {
		// Non-fatal: config was written, samba may not be running yet.
		_ = err
	}
	// Make the share path world-accessible so SMB clients can read and write.
	_ = system.ChmodSharePath(req.Path)
	// Apply (or skip) Windows ACL ZFS dataset properties.
	_ = system.SetWindowsACLDatasetProps(req.Path, req.WindowsACL)

	sess := MustSession(r)
	audit.Log(audit.Entry{
		User:   sess.Username,
		Role:   sess.Role,
		Action: audit.ActionCreateShare,
		Target: req.Name,
		Result: audit.ResultOK,
	})
	go alerts.Send(
		alerts.EventShareCreated,
		"SMB share created: "+req.Name,
		"SMB Share Created",
		"SMB share '"+req.Name+"' (path: "+req.Path+") was created by "+sess.Username+".",
	)
	jsonCreated(w, req)
}

// HandleUpdateShare replaces an existing SMB share by name.
func HandleUpdateShare(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(mux.Vars(r)["name"])
	if name == "" {
		jsonErr(w, http.StatusBadRequest, "share name required in URL")
		return
	}

	var req system.SMBShare
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Name = name // canonical name from URL

	shares, err := system.ListSMBShares(config.Dir())
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	found := false
	for i, s := range shares {
		if strings.EqualFold(s.Name, name) {
			shares[i] = req
			found = true
			break
		}
	}
	if !found {
		jsonErr(w, http.StatusNotFound, "share not found")
		return
	}

	if err := system.SaveSMBShares(config.Dir(), shares); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if r.URL.Query().Get("restart") == "true" {
		_ = system.RestartSamba()
	} else {
		_ = system.ReloadSamba()
	}
	// Apply (or revert) Windows ACL ZFS dataset properties.
	_ = system.SetWindowsACLDatasetProps(req.Path, req.WindowsACL)

	sess := MustSession(r)
	audit.Log(audit.Entry{
		User:   sess.Username,
		Role:   sess.Role,
		Action: audit.ActionEnableShare,
		Target: name,
		Result: audit.ResultOK,
		Details: "updated",
	})
	jsonOK(w, req)
}

// HandleSetSMBPassword creates a Linux system account (if needed) and sets
// the Samba password for a portal user.
func HandleSetSMBPassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || req.Password == "" {
		jsonErr(w, http.StatusBadRequest, "username and password are required")
		return
	}
	if err := system.EnsureSambaUser(req.Username, req.Password); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	sess := MustSession(r)
	audit.Log(audit.Entry{
		User:    sess.Username,
		Role:    sess.Role,
		Action:  audit.ActionUpdateUser,
		Target:  req.Username,
		Result:  audit.ResultOK,
		Details: "smb password set",
	})
	jsonOK(w, map[string]string{"message": "SMB password set for " + req.Username})
}

// HandleCleanShareRecycleBin immediately runs the recycle-bin cleanup for one share.
func HandleCleanShareRecycleBin(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(mux.Vars(r)["name"])
	if name == "" {
		jsonErr(w, http.StatusBadRequest, "share name required in URL")
		return
	}
	if err := system.CleanShareRecycleBin(config.Dir(), name); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	sess := MustSession(r)
	audit.Log(audit.Entry{
		User:    sess.Username,
		Role:    sess.Role,
		Action:  audit.ActionEnableShare,
		Target:  name,
		Result:  audit.ResultOK,
		Details: "recycle bin cleaned manually",
	})
	jsonOK(w, map[string]string{"message": "recycle bin cleaned"})
}

// HandleDeleteShare removes an SMB share by name.
func HandleDeleteShare(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(mux.Vars(r)["name"])
	if name == "" {
		jsonErr(w, http.StatusBadRequest, "share name required in URL")
		return
	}

	shares, err := system.ListSMBShares(config.Dir())
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	newShares := shares[:0]
	found := false
	for _, s := range shares {
		if strings.EqualFold(s.Name, name) {
			found = true
			continue
		}
		newShares = append(newShares, s)
	}
	if !found {
		jsonErr(w, http.StatusNotFound, "share not found")
		return
	}

	if err := system.SaveSMBShares(config.Dir(), newShares); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := system.ReloadSamba(); err != nil {
		_ = err
	}

	sess := MustSession(r)
	audit.Log(audit.Entry{
		User:   sess.Username,
		Role:   sess.Role,
		Action: audit.ActionDeleteShare,
		Target: name,
		Result: audit.ResultOK,
	})
	go alerts.Send(
		alerts.EventShareCreated,
		"SMB share deleted: "+name,
		"SMB Share Deleted",
		"SMB share '"+name+"' was deleted by "+sess.Username+".",
	)
	jsonOK(w, map[string]string{"message": "share deleted"})
}
