package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"zfsnas/internal/audit"
	"zfsnas/system"

	"github.com/gorilla/mux"
)

// HandleListLXDNetworks returns all LXD networks with detail.
// GET /api/lxd/network-bridges
func HandleListLXDNetworks(w http.ResponseWriter, r *http.Request) {
	nets, err := system.ListLXDNetworks()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, nets)
}

// HandleGetLXDNetwork returns detail for a single LXD network.
// GET /api/lxd/network-bridges/{name}
func HandleGetLXDNetwork(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	net, err := system.GetLXDNetwork(name)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, net)
}

// HandleCreateLXDNetwork creates a new LXD bridge network.
// POST /api/lxd/network-bridges
func HandleCreateLXDNetwork(w http.ResponseWriter, r *http.Request) {
	sess := MustSession(r)
	var req system.LXDNetworkCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := system.CreateLXDNetwork(req); err != nil {
		audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDNetCreate, Target: req.Name, Result: audit.ResultError, Details: err.Error()})
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDNetCreate, Target: req.Name, Result: audit.ResultOK})
	jsonOK(w, map[string]string{"ok": "created"})
}

// HandleEditLXDNetwork updates an existing LXD network.
// PUT /api/lxd/network-bridges/{name}
func HandleEditLXDNetwork(w http.ResponseWriter, r *http.Request) {
	sess := MustSession(r)
	name := mux.Vars(r)["name"]
	var req system.LXDNetworkEditRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Name = name
	if err := system.EditLXDNetwork(req); err != nil {
		audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDNetEdit, Target: name, Result: audit.ResultError, Details: err.Error()})
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDNetEdit, Target: name, Result: audit.ResultOK})
	jsonOK(w, map[string]string{"ok": "updated"})
}

// HandleDeleteLXDNetwork deletes an LXD network.
// DELETE /api/lxd/network-bridges/{name}
func HandleDeleteLXDNetwork(w http.ResponseWriter, r *http.Request) {
	sess := MustSession(r)
	name := mux.Vars(r)["name"]
	if err := system.DeleteLXDNetwork(name); err != nil {
		audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDNetDelete, Target: name, Result: audit.ResultError, Details: err.Error()})
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDNetDelete, Target: name, Result: audit.ResultOK})
	jsonOK(w, map[string]string{"ok": "deleted"})
}

// HandleGetBridgeMembers returns instances attached to an LXD bridge with their IPs.
// GET /api/lxd/network-bridges/{name}/members
func HandleGetBridgeMembers(w http.ResponseWriter, r *http.Request) {
	name := mux.Vars(r)["name"]
	members, err := system.GetBridgeMembers(name)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if members == nil {
		members = []system.BridgeMember{}
	}
	jsonOK(w, members)
}

// HandleDeleteVLANInterface removes a ZNAS-managed kernel VLAN sub-interface.
// DELETE /api/lxd/vlan-interface/{name}
func HandleDeleteVLANInterface(w http.ResponseWriter, r *http.Request) {
	sess := MustSession(r)
	name := mux.Vars(r)["name"]
	if err := system.DeleteVLANInterface(name); err != nil {
		audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDNetDelete, Target: name, Result: audit.ResultError, Details: err.Error()})
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDNetDelete, Target: name, Result: audit.ResultOK})
	jsonOK(w, map[string]string{"ok": "deleted"})
}

// HandleListPhysicalInterfaces returns host network interfaces suitable as
// a bridge parent (physical + existing host bridges, no virtual/loopback).
// GET /api/lxd/host-interfaces
func HandleListPhysicalInterfaces(w http.ResponseWriter, r *http.Request) {
	ifaces, err := system.ListPhysicalInterfaces()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, ifaces)
}

// HandleSetInterfaceMTU sets the MTU on a host network interface.
// PUT /api/lxd/host-interfaces/{name}/mtu
func HandleSetInterfaceMTU(w http.ResponseWriter, r *http.Request) {
	sess := MustSession(r)
	name := mux.Vars(r)["name"]
	var req struct {
		MTU int `json:"mtu"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := system.SetInterfaceMTU(name, req.MTU); err != nil {
		audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDNetEdit, Target: name, Result: audit.ResultError, Details: err.Error()})
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	audit.Log(audit.Entry{User: sess.Username, Role: sess.Role, Action: audit.ActionLXDNetEdit, Target: name, Result: audit.ResultOK, Details: "mtu=" + fmt.Sprintf("%d", req.MTU)})
	jsonOK(w, map[string]string{"ok": "mtu updated"})
}
