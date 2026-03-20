package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"zfsnas/internal/audit"
	"zfsnas/internal/config"
	"zfsnas/system"
)

// HandleMinIOStatus returns prereq/enabled/service/bucket status.
// GET /api/minio/status
func HandleMinIOStatus(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		prereqs := system.MinIOPrereqsInstalled()
		svc := system.GetMinIOServiceStatus()
		configured := appCfg.MinIO.RootUser != "" && appCfg.MinIO.DataDir != ""
		jsonOK(w, map[string]interface{}{
			"prereqs_installed": prereqs,
			"enabled":           appCfg.MinIO.Enabled,
			"configured":        configured,
			"service_active":    svc.Active,
			"service_status":    svc.Status,
			"bucket_count":      len(appCfg.MinIO.Buckets),
			"console_port":      appCfg.MinIO.ConsolePort,
			"tls":               appCfg.MinIO.TLS,
			"hide_nav":          appCfg.MinIO.HideNav,
		})
	}
}

// HandleMinIOServiceAction starts/stops/restarts the MinIO service.
// POST /api/minio/service
func HandleMinIOServiceAction(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		var err error
		switch req.Action {
		case "start":
			err = system.StartMinIOService()
		case "stop":
			err = system.StopMinIOService()
		case "restart":
			err = system.RestartMinIOService()
		default:
			jsonErr(w, http.StatusBadRequest, "invalid action")
			return
		}
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		jsonOK(w, map[string]bool{"ok": true})
	}
}

// HandleGetMinIOConfig returns the MinIO configuration (password redacted).
// GET /api/minio/config
func HandleGetMinIOConfig(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		jsonOK(w, map[string]interface{}{
			"enabled":      appCfg.MinIO.Enabled,
			"dataset_path": appCfg.MinIO.DatasetPath,
			"data_dir":     appCfg.MinIO.DataDir,
			"port":         appCfg.MinIO.Port,
			"console_port": appCfg.MinIO.ConsolePort,
			"tls":          appCfg.MinIO.TLS,
			"root_user":    appCfg.MinIO.RootUser,
			"region":       appCfg.MinIO.Region,
			"site_name":    appCfg.MinIO.SiteName,
			"server_url":   appCfg.MinIO.ServerURL,
		})
	}
}

// HandleSaveMinIOConfig saves the MinIO configuration and applies it.
// POST /api/minio/config
func HandleSaveMinIOConfig(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			DatasetPath  string `json:"dataset_path"`
			Port         int    `json:"port"`
			ConsolePort  int    `json:"console_port"`
			TLS          bool   `json:"tls"`
			RootUser     string `json:"root_user"`
			RootPassword string `json:"root_password"`
			Region       string `json:"region"`
			SiteName     string `json:"site_name"`
			ServerURL    string `json:"server_url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}

		req.DatasetPath = strings.TrimSpace(req.DatasetPath)
		req.RootUser = strings.TrimSpace(req.RootUser)
		req.Region = strings.TrimSpace(req.Region)

		if req.DatasetPath == "" {
			jsonErr(w, http.StatusBadRequest, "dataset_path is required")
			return
		}
		if req.Port < 1 || req.Port > 65535 {
			jsonErr(w, http.StatusBadRequest, "port must be between 1 and 65535")
			return
		}
		if req.ConsolePort < 1 || req.ConsolePort > 65535 {
			jsonErr(w, http.StatusBadRequest, "console_port must be between 1 and 65535")
			return
		}
		if req.Port == req.ConsolePort {
			jsonErr(w, http.StatusBadRequest, "port and console_port must be different")
			return
		}
		if len(req.RootUser) < 3 {
			jsonErr(w, http.StatusBadRequest, "root_user must be at least 3 characters")
			return
		}
		if req.RootPassword != "" && len(req.RootPassword) < 8 {
			jsonErr(w, http.StatusBadRequest, "root_password must be at least 8 characters")
			return
		}
		if req.Region == "" {
			req.Region = "us-east-1"
		}

		// Resolve the dataset mountpoint.
		datasets, err := system.ListAllDatasets()
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, "could not list datasets: "+err.Error())
			return
		}
		dataDir := ""
		for _, ds := range datasets {
			if ds.Name == req.DatasetPath {
				dataDir = ds.Mountpoint
				break
			}
		}
		if dataDir == "" || dataDir == "none" || dataDir == "legacy" {
			jsonErr(w, http.StatusBadRequest, "dataset not found or has no mountpoint: "+req.DatasetPath)
			return
		}

		// Keep existing password if not provided.
		password := req.RootPassword
		if password == "" {
			password = appCfg.MinIO.RootPassword
		}
		if password == "" {
			jsonErr(w, http.StatusBadRequest, "root_password is required for initial configuration")
			return
		}

		appCfg.MinIO.DatasetPath = req.DatasetPath
		appCfg.MinIO.DataDir = dataDir
		appCfg.MinIO.Port = req.Port
		appCfg.MinIO.ConsolePort = req.ConsolePort
		appCfg.MinIO.TLS = req.TLS
		appCfg.MinIO.RootUser = req.RootUser
		appCfg.MinIO.RootPassword = password
		appCfg.MinIO.Region = req.Region
		appCfg.MinIO.SiteName = strings.TrimSpace(req.SiteName)
		appCfg.MinIO.ServerURL = strings.TrimSpace(req.ServerURL)
		appCfg.MinIO.Enabled = true

		if err := config.SaveAppConfig(appCfg); err != nil {
			jsonErr(w, http.StatusInternalServerError, "save config: "+err.Error())
			return
		}

		if err := system.ApplyMinIOConfig(&appCfg.MinIO); err != nil {
			jsonErr(w, http.StatusInternalServerError, "apply config: "+err.Error())
			return
		}

		sess := MustSession(r)
		audit.Log(audit.Entry{
			User:   sess.Username,
			Role:   sess.Role,
			Action: audit.ActionEditMinIOConfig,
			Result: audit.ResultOK,
		})

		jsonOK(w, map[string]bool{"ok": true})
	}
}

// HandleListS3Users returns all MinIO IAM users.
// GET /api/minio/users
func HandleListS3Users(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !system.MinIOPrereqsInstalled() {
			jsonErr(w, http.StatusServiceUnavailable, "MinIO not installed")
			return
		}
		users, err := system.ListS3Users()
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		jsonOK(w, users)
	}
}

// HandleCreateS3User creates a new MinIO IAM user.
// POST /api/minio/user/create
func HandleCreateS3User(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			AccessKey string `json:"access_key"`
			SecretKey string `json:"secret_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		req.AccessKey = strings.TrimSpace(req.AccessKey)
		if len(req.AccessKey) < 3 {
			jsonErr(w, http.StatusBadRequest, "access_key must be at least 3 characters")
			return
		}
		if len(req.SecretKey) < 8 {
			jsonErr(w, http.StatusBadRequest, "secret_key must be at least 8 characters")
			return
		}

		if err := system.CreateS3User(req.AccessKey, req.SecretKey); err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}

		sess := MustSession(r)
		audit.Log(audit.Entry{
			User:   sess.Username,
			Role:   sess.Role,
			Action: audit.ActionCreateS3User,
			Target: req.AccessKey,
			Result: audit.ResultOK,
		})

		jsonOK(w, map[string]bool{"ok": true})
	}
}

// HandleDeleteS3User removes a MinIO IAM user.
// POST /api/minio/user/delete
func HandleDeleteS3User(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			AccessKey string `json:"access_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		req.AccessKey = strings.TrimSpace(req.AccessKey)
		if req.AccessKey == "" {
			jsonErr(w, http.StatusBadRequest, "access_key is required")
			return
		}

		if err := system.DeleteS3User(req.AccessKey); err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}

		// Remove this key from all bucket user lists in config.
		changed := false
		for i := range appCfg.MinIO.Buckets {
			filtered := appCfg.MinIO.Buckets[i].UserKeys[:0]
			for _, k := range appCfg.MinIO.Buckets[i].UserKeys {
				if k != req.AccessKey {
					filtered = append(filtered, k)
				}
			}
			if len(filtered) != len(appCfg.MinIO.Buckets[i].UserKeys) {
				appCfg.MinIO.Buckets[i].UserKeys = filtered
				changed = true
			}
		}
		if changed {
			_ = config.SaveAppConfig(appCfg)
		}

		sess := MustSession(r)
		audit.Log(audit.Entry{
			User:   sess.Username,
			Role:   sess.Role,
			Action: audit.ActionDeleteS3User,
			Target: req.AccessKey,
			Result: audit.ResultOK,
		})

		jsonOK(w, map[string]bool{"ok": true})
	}
}

// HandleSetS3UserStatus enables or disables a MinIO IAM user.
// POST /api/minio/user/status
func HandleSetS3UserStatus(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			AccessKey string `json:"access_key"`
			Enabled   bool   `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if strings.TrimSpace(req.AccessKey) == "" {
			jsonErr(w, http.StatusBadRequest, "access_key is required")
			return
		}
		if err := system.SetS3UserStatus(req.AccessKey, req.Enabled); err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		jsonOK(w, map[string]bool{"ok": true})
	}
}

// HandleSetS3UserPassword updates a MinIO IAM user's secret key.
// POST /api/minio/user/password
func HandleSetS3UserPassword(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			AccessKey string `json:"access_key"`
			SecretKey string `json:"secret_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		if strings.TrimSpace(req.AccessKey) == "" {
			jsonErr(w, http.StatusBadRequest, "access_key is required")
			return
		}
		if len(req.SecretKey) < 8 {
			jsonErr(w, http.StatusBadRequest, "secret_key must be at least 8 characters")
			return
		}
		if err := system.SetS3UserPassword(req.AccessKey, req.SecretKey); err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		jsonOK(w, map[string]bool{"ok": true})
	}
}

// HandleListS3Buckets returns the buckets tracked in portal config.
// GET /api/minio/buckets
func HandleListS3Buckets(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		buckets := appCfg.MinIO.Buckets
		if buckets == nil {
			buckets = []config.S3Bucket{}
		}
		jsonOK(w, buckets)
	}
}

// HandleCreateS3Bucket creates a new S3 bucket in MinIO and records it in config.
// POST /api/minio/bucket/create
func HandleCreateS3Bucket(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name       string   `json:"name"`
			Comment    string   `json:"comment"`
			Versioning string   `json:"versioning"`
			ObjectLock bool     `json:"object_lock"`
			Quota      string   `json:"quota"`
			AnonAccess string   `json:"anon_access"`
			UserKeys   []string `json:"user_keys"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}

		req.Name = strings.TrimSpace(strings.ToLower(req.Name))
		if err := validateS3BucketName(req.Name); err != nil {
			jsonErr(w, http.StatusBadRequest, err.Error())
			return
		}
		if req.Versioning == "" {
			req.Versioning = "off"
		}
		if req.AnonAccess == "" {
			req.AnonAccess = "none"
		}
		if req.ObjectLock && req.Versioning != "enabled" {
			jsonErr(w, http.StatusBadRequest, "object_lock requires versioning to be enabled")
			return
		}
		if req.UserKeys == nil {
			req.UserKeys = []string{}
		}

		opts := system.S3BucketCreateOptions{
			Name:       req.Name,
			Versioning: req.Versioning,
			ObjectLock: req.ObjectLock,
			Quota:      strings.TrimSpace(req.Quota),
			AnonAccess: req.AnonAccess,
		}
		if err := system.CreateS3Bucket(opts); err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}

		if len(req.UserKeys) > 0 {
			_ = system.ApplyBucketUserPolicy(req.Name, req.UserKeys, nil)
		}

		bucket := config.S3Bucket{
			Name:       req.Name,
			Comment:    strings.TrimSpace(req.Comment),
			Versioning: req.Versioning,
			ObjectLock: req.ObjectLock,
			Quota:      strings.TrimSpace(req.Quota),
			AnonAccess: req.AnonAccess,
			UserKeys:   req.UserKeys,
			CreatedAt:  time.Now().Unix(),
		}
		appCfg.MinIO.Buckets = append(appCfg.MinIO.Buckets, bucket)
		if err := config.SaveAppConfig(appCfg); err != nil {
			jsonErr(w, http.StatusInternalServerError, "save config: "+err.Error())
			return
		}

		sess := MustSession(r)
		audit.Log(audit.Entry{
			User:   sess.Username,
			Role:   sess.Role,
			Action: audit.ActionCreateS3Bucket,
			Target: req.Name,
			Result: audit.ResultOK,
		})

		jsonOK(w, map[string]bool{"ok": true})
	}
}

// HandleDeleteS3Bucket removes a bucket from MinIO and from portal config.
// POST /api/minio/bucket/delete
func HandleDeleteS3Bucket(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" {
			jsonErr(w, http.StatusBadRequest, "name is required")
			return
		}

		if err := system.DeleteS3Bucket(req.Name); err != nil {
			jsonErr(w, http.StatusInternalServerError, err.Error())
			return
		}

		newBuckets := make([]config.S3Bucket, 0, len(appCfg.MinIO.Buckets))
		for _, b := range appCfg.MinIO.Buckets {
			if b.Name != req.Name {
				newBuckets = append(newBuckets, b)
			}
		}
		appCfg.MinIO.Buckets = newBuckets
		if err := config.SaveAppConfig(appCfg); err != nil {
			jsonErr(w, http.StatusInternalServerError, "save config: "+err.Error())
			return
		}

		sess := MustSession(r)
		audit.Log(audit.Entry{
			User:   sess.Username,
			Role:   sess.Role,
			Action: audit.ActionDeleteS3Bucket,
			Target: req.Name,
			Result: audit.ResultOK,
		})

		jsonOK(w, map[string]bool{"ok": true})
	}
}

// HandleEditS3Bucket updates mutable bucket properties and re-applies the IAM policy.
// POST /api/minio/bucket/edit
func HandleEditS3Bucket(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name       string   `json:"name"`
			Comment    string   `json:"comment"`
			Versioning string   `json:"versioning"`
			Quota      string   `json:"quota"`
			AnonAccess string   `json:"anon_access"`
			UserKeys   []string `json:"user_keys"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if req.Name == "" {
			jsonErr(w, http.StatusBadRequest, "name is required")
			return
		}
		if req.UserKeys == nil {
			req.UserKeys = []string{}
		}
		if req.AnonAccess == "" {
			req.AnonAccess = "none"
		}

		idx := -1
		for i, b := range appCfg.MinIO.Buckets {
			if b.Name == req.Name {
				idx = i
				break
			}
		}
		if idx < 0 {
			jsonErr(w, http.StatusNotFound, "bucket not found")
			return
		}
		prevKeys := appCfg.MinIO.Buckets[idx].UserKeys

		target := "zfsnas/" + req.Name
		newQuota := strings.TrimSpace(req.Quota)

		_ = system.S3ApplyVersioning(target, req.Versioning)
		_ = system.S3ApplyQuota(target, newQuota)
		_ = system.S3ApplyAnonAccess(target, req.AnonAccess)
		_ = system.ApplyBucketUserPolicy(req.Name, req.UserKeys, prevKeys)

		appCfg.MinIO.Buckets[idx].Comment = strings.TrimSpace(req.Comment)
		appCfg.MinIO.Buckets[idx].Versioning = req.Versioning
		appCfg.MinIO.Buckets[idx].Quota = newQuota
		appCfg.MinIO.Buckets[idx].AnonAccess = req.AnonAccess
		appCfg.MinIO.Buckets[idx].UserKeys = req.UserKeys

		if err := config.SaveAppConfig(appCfg); err != nil {
			jsonErr(w, http.StatusInternalServerError, "save config: "+err.Error())
			return
		}

		sess := MustSession(r)
		audit.Log(audit.Entry{
			User:   sess.Username,
			Role:   sess.Role,
			Action: audit.ActionEditS3Bucket,
			Target: req.Name,
			Result: audit.ResultOK,
		})

		jsonOK(w, map[string]bool{"ok": true})
	}
}

func validateS3BucketName(name string) error {
	if len(name) < 3 || len(name) > 63 {
		return fmt.Errorf("bucket name must be 3–63 characters")
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return fmt.Errorf("bucket name must not start or end with a hyphen")
	}
	if strings.Contains(name, "--") {
		return fmt.Errorf("bucket name must not contain consecutive hyphens")
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return fmt.Errorf("bucket name may only contain lowercase letters, digits, and hyphens")
		}
	}
	return nil
}
