package handlers

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"zfsnas/internal/alerts"
	"zfsnas/internal/audit"
	"zfsnas/internal/config"
	"zfsnas/internal/keystore"
	"zfsnas/system"

	"github.com/gorilla/mux"
)

// poolNameFromDatasets extracts the pool name from any dataset path.
func poolFromAny(name string) string {
	return strings.SplitN(name, "/", 2)[0]
}

func HandleListDatasets(w http.ResponseWriter, r *http.Request) {
	datasets, err := system.ListAllDatasets()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if datasets == nil {
		datasets = []system.Dataset{}
	}
	jsonOK(w, datasets)
}

func HandleCreateDataset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name            string `json:"name"`
		Quota           uint64 `json:"quota"`
		QuotaType       string `json:"quota_type"`
		Refreservation  uint64 `json:"refreservation"`
		Compression     string `json:"compression"`
		Sync            string `json:"sync"`
		Dedup           string `json:"dedup"`
		CaseSensitivity string `json:"case_sensitivity"`
		RecordSize      string `json:"record_size"`
		Comment         string `json:"comment"`
		KeyID           string `json:"key_id"`
		ClientKeyHex    string `json:"client_key_hex"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || !strings.Contains(req.Name, "/") {
		jsonErr(w, http.StatusBadRequest, "dataset name must include pool (e.g. tank/data)")
		return
	}
	if req.QuotaType == "" {
		req.QuotaType = "quota"
	}
	if req.Compression == "" {
		req.Compression = "inherit"
	}

	// Validate: only one of key_id or client_key_hex may be set.
	if req.KeyID != "" && req.ClientKeyHex != "" {
		jsonErr(w, http.StatusBadRequest, "only one of key_id or client_key_hex may be set")
		return
	}

	var keyFilePath string
	if req.KeyID != "" {
		if !keystore.Exists(req.KeyID) {
			jsonErr(w, http.StatusBadRequest, "encryption key not found")
			return
		}
		keyFilePath = keystore.KeyFilePath(req.KeyID)
	}

	// Validate client key hex: must be exactly 64 hex chars (32 bytes).
	clientKeyHex := strings.TrimSpace(req.ClientKeyHex)
	if clientKeyHex != "" && len(clientKeyHex) != 64 {
		jsonErr(w, http.StatusBadRequest, "client_key_hex must be exactly 64 hex characters (32 bytes)")
		return
	}

	opts := system.DatasetCreateOptions{
		Quota:           req.Quota,
		QuotaType:       req.QuotaType,
		Refreservation:  req.Refreservation,
		Compression:     req.Compression,
		Sync:            req.Sync,
		Dedup:           req.Dedup,
		CaseSensitivity: req.CaseSensitivity,
		RecordSize:      req.RecordSize,
		Comment:         strings.TrimSpace(req.Comment),
		KeyFilePath:     keyFilePath,
		ClientKeyHex:    clientKeyHex,
	}
	if err := system.CreateDataset(req.Name, opts); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	sess := MustSession(r)
	audit.Log(audit.Entry{
		User:    sess.Username,
		Role:    sess.Role,
		Action:  audit.ActionCreateDataset,
		Target:  req.Name,
		Result:  audit.ResultOK,
		Details: "compression=" + req.Compression,
	})
	go alerts.Send(
		alerts.EventPoolActions,
		"Dataset created: "+req.Name,
		"ZFS Dataset Created",
		"Dataset '"+req.Name+"' was created by "+sess.Username+".",
	)

	jsonCreated(w, map[string]string{"name": req.Name})
}

func HandleUpdateDataset(w http.ResponseWriter, r *http.Request) {
	path := mux.Vars(r)["path"]
	if path == "" {
		jsonErr(w, http.StatusBadRequest, "dataset path required")
		return
	}

	var req struct {
		Quota          *uint64 `json:"quota"`
		QuotaType      string  `json:"quota_type"`
		Refreservation *uint64 `json:"refreservation"`
		Compression    string  `json:"compression"`
		Sync           string  `json:"sync"`
		Dedup          string  `json:"dedup"`
		RecordSize     string  `json:"record_size"`
		Comment        *string `json:"comment"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}

	props := map[string]string{}
	if req.Quota != nil {
		qt := "quota"
		if req.QuotaType == "refquota" {
			qt = "refquota"
		}
		if *req.Quota == 0 {
			props[qt] = "none"
		} else {
			props[qt] = strconv.FormatUint(*req.Quota, 10)
		}
	}
	if req.Refreservation != nil {
		if *req.Refreservation == 0 {
			props["refreservation"] = "none"
		} else {
			props["refreservation"] = strconv.FormatUint(*req.Refreservation, 10)
		}
	}
	if req.Compression != "" {
		props["compression"] = req.Compression
	}
	if req.Sync != "" {
		props["sync"] = req.Sync
	}
	if req.Dedup != "" {
		props["dedup"] = req.Dedup
	}
	if req.RecordSize != "" {
		if req.RecordSize == "inherit" {
			props["recordsize"] = "inherit"
		} else {
			props["recordsize"] = req.RecordSize
		}
	}
	if req.Comment != nil {
		// Empty string clears via `zfs inherit`; non-empty sets the property.
		props["zfsnas:comment"] = strings.TrimSpace(*req.Comment)
	}

	if len(props) == 0 {
		jsonErr(w, http.StatusBadRequest, "nothing to update")
		return
	}

	if err := system.SetDatasetProps(path, props); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	sess := MustSession(r)
	audit.Log(audit.Entry{
		User:   sess.Username,
		Role:   sess.Role,
		Action: audit.ActionUpdateDataset,
		Target: path,
		Result: audit.ResultOK,
	})

	jsonOK(w, map[string]string{"message": "dataset updated"})
}

// HandleRenameDataset renames a dataset or zvol via `zfs rename`.
// POST /api/datasets/rename
func HandleRenameDataset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldName string `json:"old_name"`
		NewName string `json:"new_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := system.RenameDataset(req.OldName, req.NewName); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	sess := MustSession(r)
	audit.Log(audit.Entry{
		User:   sess.Username,
		Role:   sess.Role,
		Action: audit.ActionUpdateDataset,
		Target: req.OldName + " → " + req.NewName,
		Result: audit.ResultOK,
	})
	jsonOK(w, map[string]interface{}{"ok": true})
}

func HandleDeleteDataset(w http.ResponseWriter, r *http.Request) {
	path := mux.Vars(r)["path"]
	if path == "" {
		jsonErr(w, http.StatusBadRequest, "dataset path required")
		return
	}
	// Prevent deleting the pool root.
	if !strings.Contains(path, "/") {
		jsonErr(w, http.StatusBadRequest, "cannot delete pool root dataset")
		return
	}

	recursive := r.URL.Query().Get("recursive") == "true"
	force := r.URL.Query().Get("force") == "true"
	var destroyErr error
	switch {
	case force:
		destroyErr = system.DestroyDatasetForce(path)
	case recursive:
		destroyErr = system.DestroyDatasetRecursive(path)
	default:
		destroyErr = system.DestroyDataset(path)
	}
	if destroyErr != nil {
		jsonErr(w, http.StatusInternalServerError, destroyErr.Error())
		return
	}

	sess := MustSession(r)
	audit.Log(audit.Entry{
		User:   sess.Username,
		Role:   sess.Role,
		Action: audit.ActionDeleteDataset,
		Target: path,
		Result: audit.ResultOK,
	})
	go alerts.Send(
		alerts.EventPoolActions,
		"Dataset deleted: "+path,
		"ZFS Dataset Deleted",
		"Dataset '"+path+"' was deleted by "+sess.Username+".",
	)

	jsonOK(w, map[string]string{"message": "dataset deleted"})
}

// HandleLoadDatasetKey loads an encryption key for a locked dataset and mounts it.
// Body: {"key_id": "<uuid>"}
func HandleLoadDatasetKey(w http.ResponseWriter, r *http.Request) {
	path := mux.Vars(r)["path"]
	if path == "" {
		jsonErr(w, http.StatusBadRequest, "dataset path required")
		return
	}
	var req struct {
		KeyID string `json:"key_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.KeyID == "" {
		jsonErr(w, http.StatusBadRequest, "key_id is required")
		return
	}
	if !keystore.Exists(req.KeyID) {
		jsonErr(w, http.StatusBadRequest, "key not found")
		return
	}
	keyPath := keystore.KeyFilePath(req.KeyID)
	if err := system.LoadPoolKey(path, keyPath); err != nil {
		jsonErr(w, http.StatusInternalServerError, "failed to load key: "+err.Error())
		return
	}
	// Persist the keylocation so autoLoadEncryptionKeys can reload it after reboot.
	if err := system.SetDatasetProps(path, map[string]string{
		"keylocation": "file://" + keyPath,
	}); err != nil {
		// Non-fatal: key is loaded now, just won't survive reboot.
		_ = err
	}
	if err := system.MountDataset(path); err != nil {
		jsonErr(w, http.StatusInternalServerError, "key loaded but mount failed: "+err.Error())
		return
	}
	// Mount any unlocked child datasets that weren't auto-mounted.
	system.MountUnlockedChildren(path)
	sess := MustSession(r)
	audit.Log(audit.Entry{
		User:   sess.Username,
		Role:   sess.Role,
		Action: audit.ActionLoadKey,
		Target: path,
		Result: audit.ResultOK,
	})
	jsonOK(w, map[string]string{"message": "key loaded and dataset mounted"})
}

// HandleUnloadDatasetKey unmounts a dataset and unloads its encryption key (locks it).
// Body (optional): {"force": true} — force-disconnects active clients (e.g. SMB sessions).
func HandleUnloadDatasetKey(w http.ResponseWriter, r *http.Request) {
	path := mux.Vars(r)["path"]
	if path == "" {
		jsonErr(w, http.StatusBadRequest, "dataset path required")
		return
	}
	var req struct {
		Force bool `json:"force"`
	}
	// Body is optional; ignore decode errors.
	_ = json.NewDecoder(r.Body).Decode(&req)

	if err := system.UnloadDatasetKey(path, req.Force, config.Dir()); err != nil {
		// Return 409 Conflict when the dataset is busy so the UI can offer a force option.
		if strings.Contains(err.Error(), "dataset is busy") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": err.Error(),
				"code":  "busy",
			})
			return
		}
		jsonErr(w, http.StatusInternalServerError, "failed to lock dataset: "+err.Error())
		return
	}
	details := ""
	if req.Force {
		details = "force"
	}
	sess := MustSession(r)
	audit.Log(audit.Entry{
		User:    sess.Username,
		Role:    sess.Role,
		Action:  audit.ActionLockDataset,
		Target:  path,
		Result:  audit.ResultOK,
		Details: details,
	})
	jsonOK(w, map[string]string{"message": "dataset locked"})
}

// HandleUnlockDatasetPassphrase loads a dataset encryption key supplied as a
// passphrase/hex string in the request body. The passphrase is piped directly
// to zfs load-key via stdin and is never stored or logged.
// Body: {"passphrase": "..."}
func HandleUnlockDatasetPassphrase(w http.ResponseWriter, r *http.Request) {
	path := mux.Vars(r)["path"]
	if path == "" {
		jsonErr(w, http.StatusBadRequest, "dataset path required")
		return
	}
	var req struct {
		Passphrase string `json:"passphrase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Passphrase) == "" {
		jsonErr(w, http.StatusBadRequest, "passphrase is required")
		return
	}
	if err := system.LoadDatasetKeyPassphrase(path, strings.TrimSpace(req.Passphrase)); err != nil {
		jsonErr(w, http.StatusInternalServerError, "failed to unlock dataset: "+err.Error())
		return
	}
	if err := system.MountDataset(path); err != nil {
		jsonErr(w, http.StatusInternalServerError, "key loaded but mount failed: "+err.Error())
		return
	}
	system.MountUnlockedChildren(path)
	sess := MustSession(r)
	audit.Log(audit.Entry{
		User:   sess.Username,
		Role:   sess.Role,
		Action: audit.ActionUnlockDataset,
		Target: path,
		Result: audit.ResultOK,
	})
	jsonOK(w, map[string]string{"message": "dataset unlocked and mounted"})
}

// HandleGetDatasetKeyInfo returns the keylocation and key_locked status for a dataset.
func HandleGetDatasetKeyInfo(w http.ResponseWriter, r *http.Request) {
	path := mux.Vars(r)["path"]
	if path == "" {
		jsonErr(w, http.StatusBadRequest, "dataset path required")
		return
	}
	loc := system.GetKeyLocation(path)
	status := system.GetKeyStatus(path)
	jsonOK(w, map[string]interface{}{
		"keylocation": loc,
		"key_locked":  status == "unavailable",
	})
}

// HandleSetDatasetKeySource changes the keylocation for an encrypted dataset.
// Body: {"key_source": "stored", "key_id": "<uuid>"} or {"key_source": "prompt"}
func HandleSetDatasetKeySource(w http.ResponseWriter, r *http.Request) {
	path := mux.Vars(r)["path"]
	if path == "" {
		jsonErr(w, http.StatusBadRequest, "dataset path required")
		return
	}
	var req struct {
		KeySource string `json:"key_source"`
		KeyID     string `json:"key_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	switch req.KeySource {
	case "stored":
		if req.KeyID == "" {
			jsonErr(w, http.StatusBadRequest, "key_id is required for stored key source")
			return
		}
		if !keystore.Exists(req.KeyID) {
			jsonErr(w, http.StatusBadRequest, "key not found")
			return
		}
		keyPath := keystore.KeyFilePath(req.KeyID)
		props := map[string]string{
			"keylocation": "file://" + keyPath,
			"canmount":    "on",
		}
		if err := system.SetDatasetProps(path, props); err != nil {
			jsonErr(w, http.StatusInternalServerError, "failed to set key source: "+err.Error())
			return
		}
		sess := MustSession(r)
		// Look up key name for audit detail.
		keyName := req.KeyID
		if keys, err := loadEncryptionKeyByID(req.KeyID); err == nil {
			keyName = keys
		}
		audit.Log(audit.Entry{
			User:    sess.Username,
			Role:    sess.Role,
			Action:  audit.ActionChangeKeySource,
			Target:  path,
			Result:  audit.ResultOK,
			Details: "stored:" + keyName,
		})
	case "prompt":
		props := map[string]string{
			"keylocation": "prompt",
			"canmount":    "noauto",
		}
		if err := system.SetDatasetProps(path, props); err != nil {
			jsonErr(w, http.StatusInternalServerError, "failed to set key source: "+err.Error())
			return
		}
		sess := MustSession(r)
		audit.Log(audit.Entry{
			User:    sess.Username,
			Role:    sess.Role,
			Action:  audit.ActionChangeKeySource,
			Target:  path,
			Result:  audit.ResultOK,
			Details: "prompt",
		})
	default:
		jsonErr(w, http.StatusBadRequest, "key_source must be 'stored' or 'prompt'")
		return
	}
	jsonOK(w, map[string]string{"message": "key source updated"})
}

// loadEncryptionKeyByID returns the friendly name for a key ID, or the ID itself on error.
func loadEncryptionKeyByID(id string) (string, error) {
	keys, err := config.LoadEncryptionKeys()
	if err != nil {
		return id, err
	}
	for _, k := range keys {
		if k.ID == id {
			return k.Name, nil
		}
	}
	return id, nil
}
