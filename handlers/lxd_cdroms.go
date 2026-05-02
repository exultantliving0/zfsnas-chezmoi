package handlers

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gorilla/mux"
	"zfsnas/internal/audit"
	"zfsnas/system"
)

// lxdCDROMResponse describes the CD/DVD drives currently attached to a VM and
// the ISOs available in the same storage pool — payload for the in-console
// drive picker.
//
// IsVM is the trigger the toolbar uses to decide whether to render the disc
// button at all: containers don't have a CDROM concept here, but every VM
// can have a CDROM attached on demand (e.g. mid-install driver injection),
// so the toolbar shows the button even when no ISO is currently inserted.
type lxdCDROMResponse struct {
	Configured []lxdCDROMEntry     `json:"configured"`
	Pool       string              `json:"pool"`
	Available  []system.LXDISOInfo `json:"available"`
	Running    bool                `json:"running"`
	IsVM       bool                `json:"is_vm"`
}

type lxdCDROMEntry struct {
	Path     string `json:"path"`
	Filename string `json:"filename"`
}

// HandleLXDListCDROMs returns the current CD/DVD drive configuration plus the
// ISOs available in the VM's storage pool. The VGA console toolbar uses this
// to populate its drive-swap menu.
// GET /api/lxd/instances/{name}/cdroms
func HandleLXDListCDROMs(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	cfg, err := system.LXDGetConfig(name)
	if err != nil {
		jsonErr(w, http.StatusNotFound, err.Error())
		return
	}

	resp := lxdCDROMResponse{}
	for _, p := range cfg.CDROMs {
		if p == "" {
			continue
		}
		resp.Configured = append(resp.Configured, lxdCDROMEntry{
			Path:     p,
			Filename: filepath.Base(p),
		})
	}

	// Resolve the pool from the VM's root disk so we know where to list ISOs.
	for _, d := range cfg.Disks {
		if d.IsRoot && d.Pool != "" {
			resp.Pool = d.Pool
			break
		}
	}
	if resp.Pool == "" && len(cfg.Disks) > 0 {
		resp.Pool = cfg.Disks[0].Pool
	}

	if resp.Pool != "" {
		isos, err := system.LXDListISOs(resp.Pool)
		if err == nil {
			resp.Available = isos
		}
	}
	if resp.Available == nil {
		resp.Available = []system.LXDISOInfo{}
	}
	if resp.Configured == nil {
		resp.Configured = []lxdCDROMEntry{}
	}

	if status, _ := system.LXDGetStatus(name); strings.EqualFold(status, "Running") {
		resp.Running = true
	}
	resp.IsVM = cfg.IsVM

	jsonOK(w, resp)
}

// HandleLXDSwapCDROM replaces the first CD/DVD drive with the named ISO from
// the VM's storage pool, or detaches it when filename is empty. The VM must
// be restarted for the change to take effect — raw.qemu is consumed only at
// QEMU start.
// POST /api/lxd/instances/{name}/cdroms/swap
// body: {"filename": "ubuntu.iso"} or {"filename": ""}
func HandleLXDSwapCDROM(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	sess := MustSession(r)
	var req struct {
		Filename string `json:"filename"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	req.Filename = filepath.Base(strings.TrimSpace(req.Filename))
	if req.Filename == "." || req.Filename == "/" {
		req.Filename = ""
	}

	cfg, err := system.LXDGetConfig(name)
	if err != nil {
		jsonErr(w, http.StatusNotFound, err.Error())
		return
	}

	pool := ""
	for _, d := range cfg.Disks {
		if d.IsRoot && d.Pool != "" {
			pool = d.Pool
			break
		}
	}
	if pool == "" && len(cfg.Disks) > 0 {
		pool = cfg.Disks[0].Pool
	}
	if pool == "" {
		jsonErr(w, http.StatusBadRequest, "could not resolve storage pool for VM")
		return
	}

	// Build the new CDROM list: replace slot 0 with the chosen ISO (or empty),
	// keep any additional slots untouched.
	var newPaths []string
	if req.Filename != "" {
		isoDir, err := system.LXDISODir(pool)
		if err != nil {
			jsonErr(w, http.StatusBadRequest, "cannot resolve ISO directory: "+err.Error())
			return
		}
		newPaths = append(newPaths, filepath.Join(isoDir, req.Filename))
	} else {
		newPaths = append(newPaths, "")
	}
	for i, p := range cfg.CDROMs {
		if i == 0 {
			continue
		}
		newPaths = append(newPaths, p)
	}

	cfg.CDROMs = newPaths
	cfg.ApplyCDROMs = true
	if err := system.LXDSetConfig(name, cfg); err != nil {
		audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDEditConfig, Target: name, Result: audit.ResultError, Details: "cdrom swap: " + err.Error()})
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	target := req.Filename
	if target == "" {
		target = "(empty)"
	}
	audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDEditConfig, Target: name, Result: audit.ResultOK, Details: "cdrom → " + target})

	running := false
	if status, _ := system.LXDGetStatus(name); strings.EqualFold(status, "Running") {
		running = true
	}
	jsonOK(w, map[string]interface{}{
		"ok":      true,
		"running": running,
	})
}
