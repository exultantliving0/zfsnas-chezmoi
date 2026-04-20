package system

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// BridgeMember is an instance (VM or container) attached to an LXD bridge.
type BridgeMember struct {
	Name        string `json:"name"`
	Type        string `json:"type"`        // "virtual-machine" | "container"
	Status      string `json:"status"`
	Description string `json:"description"`
	DeviceName  string `json:"device_name"` // NIC device name inside the instance
	IPv4        string `json:"ipv4"`        // IP on this bridge (empty if stopped or unknown)
}

// GetBridgeMembers returns instances attached to the named LXD bridge with their IPs.
func GetBridgeMembers(bridge string) ([]BridgeMember, error) {
	// Get network detail to find used_by list.
	netOut, err := exec.Command("lxc", "query", "/1.0/networks/"+bridge).Output()
	if err != nil {
		return nil, fmt.Errorf("lxc query network: %w", err)
	}
	var net struct {
		UsedBy []string `json:"used_by"`
	}
	if err := json.Unmarshal(netOut, &net); err != nil {
		return nil, err
	}

	var members []BridgeMember
	for _, uri := range net.UsedBy {
		if !strings.Contains(uri, "/1.0/instances/") {
			continue
		}
		instName := uri[strings.LastIndex(uri, "/")+1:]

		// Get instance config: type, description, devices, expanded_config (volatile MACs).
		cfgOut, err := exec.Command("lxc", "query", "/1.0/instances/"+instName).Output()
		if err != nil {
			continue
		}
		var inst struct {
			Type           string                       `json:"type"`
			Description    string                       `json:"description"`
			Status         string                       `json:"status"`
			Devices        map[string]map[string]string `json:"devices"`
			ExpandedConfig map[string]string            `json:"expanded_config"`
		}
		if err := json.Unmarshal(cfgOut, &inst); err != nil {
			continue
		}

		// Find which LXD device name maps to this bridge and its volatile MAC.
		devName := ""
		devMAC := ""
		for dev, cfg := range inst.Devices {
			if cfg["type"] != "nic" {
				continue
			}
			if cfg["network"] == bridge || cfg["parent"] == bridge {
				devName = dev
				// MAC may be set explicitly on the device or stored as volatile.
				devMAC = cfg["hwaddr"]
				if devMAC == "" && inst.ExpandedConfig != nil {
					devMAC = inst.ExpandedConfig["volatile."+dev+".hwaddr"]
				}
				break
			}
		}

		m := BridgeMember{
			Name:        instName,
			Type:        inst.Type,
			Status:      inst.Status,
			Description: inst.Description,
			DeviceName:  devName,
		}

		// Get IP from instance state if running.
		if inst.Status == "Running" && devName != "" {
			stateOut, err := exec.Command("lxc", "query", "/1.0/instances/"+instName+"/state").Output()
			if err == nil {
				var state struct {
					Network map[string]struct {
						HWAddr    string `json:"hwaddr"`
						Addresses []struct {
							Family  string `json:"family"`
							Address string `json:"address"`
							Scope   string `json:"scope"`
						} `json:"addresses"`
					} `json:"network"`
				}
				if err := json.Unmarshal(stateOut, &state); err == nil {
					// First try direct name match (works for containers).
					// Fall back to MAC match (needed for VMs where OS may rename the NIC).
					iface, ok := state.Network[devName]
					if !ok && devMAC != "" {
						for _, netIface := range state.Network {
							if strings.EqualFold(netIface.HWAddr, devMAC) {
								iface = netIface
								ok = true
								break
							}
						}
					}
					if ok {
						for _, a := range iface.Addresses {
							if a.Family == "inet" && a.Scope == "global" {
								m.IPv4 = a.Address
								break
							}
						}
					}
				}
			}
		}

		members = append(members, m)
	}
	return members, nil
}

// LXDNetwork represents a single LXD network as returned by lxc network list/show.
type LXDNetwork struct {
	Name        string            `json:"name"`
	Type        string            `json:"type"`    // "bridge", "physical", "vlan", etc.
	Managed     bool              `json:"managed"`
	Description string            `json:"description"`
	State       string            `json:"state"`   // "Created" | ""
	IPv4        string            `json:"ipv4"`
	IPv6        string            `json:"ipv6"`
	Config      map[string]string `json:"config"`
	UsedBy      []string          `json:"used_by"` // raw /1.0/instances/... URIs
	VMCount     int               `json:"vm_count"`
}

// LXDNetworkCreateRequest holds parameters for creating a new LXD bridge network.
type LXDNetworkCreateRequest struct {
	Name            string `json:"name"`
	Description     string `json:"description"`
	BridgeType      string `json:"bridge_type"`      // "nat" | "vlan" | "plain"
	MTU             int    `json:"mtu"`              // 0 = default (1500)
	// nat fields
	IPv4Address     string `json:"ipv4_address"`     // e.g. "10.10.10.1/24"
	IPv4NAT         bool   `json:"ipv4_nat"`
	IPv6Address     string `json:"ipv6_address"`     // e.g. "fd00::1/64" or "" for none
	IPv6NAT         bool   `json:"ipv6_nat"`
	// vlan/plain fields
	ParentInterface string `json:"parent_interface"` // e.g. "enxa0cec8cd42e7"
	VLANTag         int    `json:"vlan_tag"`         // >0 = create VLAN sub-interface
	VLANIfaceName   string `json:"vlan_iface_name"`  // optional override; auto-generated if empty
}

// LXDNetworkEditRequest holds editable fields for an existing LXD network.
type LXDNetworkEditRequest struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Config      map[string]string `json:"config"`
}

// ListLXDNetworks returns all LXD networks with full detail.
func ListLXDNetworks() ([]LXDNetwork, error) {
	out, err := exec.Command("lxc", "network", "list", "--format", "json").Output()
	if err != nil {
		return nil, fmt.Errorf("lxc network list: %w", err)
	}
	var raw []struct {
		Name        string            `json:"name"`
		Type        string            `json:"type"`
		Managed     bool              `json:"managed"`
		Description string            `json:"description"`
		Status      string            `json:"status"`
		Config      map[string]string `json:"config"`
		UsedBy      []string          `json:"used_by"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, err
	}
	nets := make([]LXDNetwork, 0, len(raw))
	for _, r := range raw {
		n := LXDNetwork{
			Name:        r.Name,
			Type:        r.Type,
			Managed:     r.Managed,
			Description: r.Description,
			State:       r.Status,
			Config:      r.Config,
			UsedBy:      r.UsedBy,
		}
		if r.Config != nil {
			n.IPv4 = r.Config["ipv4.address"]
			n.IPv6 = r.Config["ipv6.address"]
		}
		for _, u := range r.UsedBy {
			if strings.Contains(u, "/1.0/instances/") {
				n.VMCount++
			}
		}
		nets = append(nets, n)
	}
	return nets, nil
}

// GetLXDNetwork returns detail for a single LXD network.
func GetLXDNetwork(name string) (LXDNetwork, error) {
	out, err := exec.Command("lxc", "query", "/1.0/networks/"+name).Output()
	if err != nil {
		return LXDNetwork{}, fmt.Errorf("lxc network show: %w", err)
	}
	var r struct {
		Name        string            `json:"name"`
		Type        string            `json:"type"`
		Managed     bool              `json:"managed"`
		Description string            `json:"description"`
		Status      string            `json:"status"`
		Config      map[string]string `json:"config"`
		UsedBy      []string          `json:"used_by"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		return LXDNetwork{}, err
	}
	n := LXDNetwork{
		Name:        r.Name,
		Type:        r.Type,
		Managed:     r.Managed,
		Description: r.Description,
		State:       r.Status,
		Config:      r.Config,
		UsedBy:      r.UsedBy,
	}
	if r.Config != nil {
		n.IPv4 = r.Config["ipv4.address"]
		n.IPv6 = r.Config["ipv6.address"]
	}
	for _, u := range r.UsedBy {
		if strings.Contains(u, "/1.0/instances/") {
			n.VMCount++
		}
	}
	return n, nil
}

// vlanIfaceName returns the kernel VLAN sub-interface name: <parent>-vlan<vid>.
// Linux interface names are limited to 15 characters (IFNAMSIZ-1), so the
// parent is truncated from the right if the full name would exceed that limit.
func vlanIfaceName(parent string, vid int) string {
	suffix := fmt.Sprintf("-v%d", vid)
	maxParent := 15 - len(suffix)
	if maxParent < 1 {
		maxParent = 1
	}
	p := parent
	if len(p) > maxParent {
		p = p[:maxParent]
	}
	return p + suffix
}

// znasManagedVLANComment is the marker written around ZNAS-created VLAN stanzas.
const znasManagedVLANStart = "# znas-managed-vlan-start"
const znasManagedVLANEnd = "# znas-managed-vlan-end"

// forceRemoveVLANKernelInterface brings down and removes a kernel VLAN interface if it exists.
func forceRemoveVLANKernelInterface(iface string) {
	data, _ := os.ReadFile("/proc/net/dev")
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), iface+":") {
			found = true
			break
		}
	}
	if !found {
		return
	}
	exec.Command("sudo", "/sbin/ip", "link", "set", iface, "down").CombinedOutput()
	exec.Command("sudo", "/sbin/ip", "link", "del", iface).CombinedOutput()
}

// writeVLANInterfaceStanza appends a VLAN sub-interface stanza to /etc/network/interfaces.
func writeVLANInterfaceStanza(parent string, vid int) error {
	return writeVLANInterfaceStanzaCustom(parent, vid, vlanIfaceName(parent, vid))
}

// writeVLANInterfaceStanzaCustom is like writeVLANInterfaceStanza but uses a caller-supplied iface name.
func writeVLANInterfaceStanzaCustom(parent string, vid int, iface string) error {

	// If the kernel interface already exists (stale from a previous run), remove it
	// so that lxc network create doesn't fail with "already exists".
	forceRemoveVLANKernelInterface(iface)

	stanza := fmt.Sprintf(`
%s name=%s vid=%d
auto %s
iface %s inet manual
    pre-up ip link add link %s name %s type vlan id %d
    post-down ip link del %s
%s
`, znasManagedVLANStart, iface, vid,
		iface, iface, parent, iface, vid, iface,
		znasManagedVLANEnd)

	// Read current file.
	existing, _ := os.ReadFile("/etc/network/interfaces")

	// Remove any existing stanza for this iface before rewriting (handles re-create).
	if strings.Contains(string(existing), "auto "+iface) {
		removeVLANInterfaceStanza(iface)
		existing, _ = os.ReadFile("/etc/network/interfaces")
	}

	newContent := string(existing) + stanza

	// Write via sudo tee.
	cmd := exec.Command("sudo", "/usr/bin/tee", "/etc/network/interfaces")
	cmd.Stdin = strings.NewReader(newContent)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("write interfaces: %s", strings.TrimSpace(string(out)))
	}

	// Bring the interface up immediately.
	if out, err := exec.Command("sudo", "/usr/sbin/ifup", iface).CombinedOutput(); err != nil {
		_ = out
	}
	return nil
}

// DeleteVLANInterface removes a kernel VLAN sub-interface and, if present, its
// ZNAS-managed /etc/network/interfaces stanza.
func DeleteVLANInterface(name string) error {
	// Remove stanza if ZNAS wrote one (best-effort; may already be gone from a
	// failed create rollback).
	existing, _ := os.ReadFile("/etc/network/interfaces")
	if strings.Contains(string(existing), " name="+name+" ") {
		removeVLANInterfaceStanza(name)
	}
	forceRemoveVLANKernelInterface(name)
	return nil
}

// removeVLANInterfaceStanza removes ZNAS-managed VLAN stanzas for the given iface
// from /etc/network/interfaces and brings the interface down.
func removeVLANInterfaceStanza(iface string) {
	existing, err := os.ReadFile("/etc/network/interfaces")
	if err != nil {
		return
	}
	content := string(existing)

	// Find and remove the znas-managed block containing this iface name.
	for {
		startIdx := strings.Index(content, znasManagedVLANStart)
		if startIdx < 0 {
			break
		}
		endIdx := strings.Index(content[startIdx:], znasManagedVLANEnd)
		if endIdx < 0 {
			break
		}
		block := content[startIdx : startIdx+endIdx+len(znasManagedVLANEnd)]
		if strings.Contains(block, " name="+iface+" ") {
			// Remove this block plus any surrounding blank lines.
			content = strings.Replace(content, block, "", 1)
			content = strings.ReplaceAll(content, "\n\n\n", "\n\n")
		} else {
			break
		}
	}

	cmd := exec.Command("sudo", "/usr/bin/tee", "/etc/network/interfaces")
	cmd.Stdin = strings.NewReader(content)
	cmd.CombinedOutput()

	// Best-effort bring-down.
	exec.Command("sudo", "/usr/sbin/ifdown", iface).CombinedOutput()
}

// setLXDNetworkDescription sets the description of an LXD network via the REST API.
// lxc network set does not accept description as a config key; PATCH /1.0/networks
// is the correct approach.
func setLXDNetworkDescription(name, description string) error {
	payload := fmt.Sprintf(`{"description":%q}`, description)
	if out, err := exec.Command("lxc", "query", "--wait", "-X", "PATCH",
		"/1.0/networks/"+name, "-d", payload).CombinedOutput(); err != nil {
		return fmt.Errorf("set description: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// CreateLXDNetwork creates a new LXD bridge network and, for VLAN-backed bridges,
// writes the necessary /etc/network/interfaces stanza.
func CreateLXDNetwork(req LXDNetworkCreateRequest) error {
	if req.Name == "" {
		return fmt.Errorf("name is required")
	}

	mtuArg := ""
	if req.MTU > 0 && req.MTU != 1500 {
		mtuArg = fmt.Sprintf("bridge.mtu=%d", req.MTU)
	}

	switch req.BridgeType {
	case "nat":
		ipv4 := req.IPv4Address
		if ipv4 == "" {
			ipv4 = "none"
		}
		ipv6 := req.IPv6Address
		if ipv6 == "" {
			ipv6 = "none"
		}
		args := []string{"network", "create", req.Name,
			"ipv4.address=" + ipv4,
			fmt.Sprintf("ipv4.nat=%v", req.IPv4NAT),
			"ipv6.address=" + ipv6,
		}
		if ipv6 != "none" {
			args = append(args, fmt.Sprintf("ipv6.nat=%v", req.IPv6NAT))
		}
		if mtuArg != "" {
			args = append(args, mtuArg)
		}
		if out, err := exec.Command("lxc", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("lxc network create: %s", strings.TrimSpace(string(out)))
		}
		if req.Description != "" {
			if err := setLXDNetworkDescription(req.Name, req.Description); err != nil {
				return err
			}
		}

	case "vlan":
		if req.ParentInterface == "" {
			return fmt.Errorf("parent interface is required for VLAN bridge")
		}
		if req.VLANTag < 1 || req.VLANTag > 4094 {
			return fmt.Errorf("VLAN tag must be 1-4094")
		}
		vlanIface := req.VLANIfaceName
		if vlanIface == "" {
			vlanIface = vlanIfaceName(req.ParentInterface, req.VLANTag)
		}
		if len(vlanIface) > 15 {
			return fmt.Errorf("VLAN interface name %q exceeds 15-character Linux limit", vlanIface)
		}
		if req.Name == vlanIface {
			return fmt.Errorf("bridge name cannot be the same as the VLAN interface name (%s)", vlanIface)
		}
		if err := writeVLANInterfaceStanzaCustom(req.ParentInterface, req.VLANTag, vlanIface); err != nil {
			return err
		}
		args := []string{"network", "create", req.Name,
			"bridge.external_interfaces=" + vlanIface,
			"ipv4.address=none",
			"ipv6.address=none",
		}
		if mtuArg != "" {
			args = append(args, mtuArg)
		}
		if out, err := exec.Command("lxc", args...).CombinedOutput(); err != nil {
			removeVLANInterfaceStanza(vlanIface)
			return fmt.Errorf("lxc network create: %s", strings.TrimSpace(string(out)))
		}
		if req.Description != "" {
			if err := setLXDNetworkDescription(req.Name, req.Description); err != nil {
				return err
			}
		}

	case "plain":
		if req.ParentInterface == "" {
			return fmt.Errorf("parent interface is required for plain bridge")
		}
		args := []string{"network", "create", req.Name,
			"bridge.external_interfaces=" + req.ParentInterface,
			"ipv4.address=none",
			"ipv6.address=none",
		}
		if mtuArg != "" {
			args = append(args, mtuArg)
		}
		if out, err := exec.Command("lxc", args...).CombinedOutput(); err != nil {
			return fmt.Errorf("lxc network create: %s", strings.TrimSpace(string(out)))
		}
		if req.Description != "" {
			if err := setLXDNetworkDescription(req.Name, req.Description); err != nil {
				return err
			}
		}

	default:
		return fmt.Errorf("unknown bridge type: %s", req.BridgeType)
	}
	return nil
}

// EditLXDNetwork updates description and config keys of an existing LXD network.
func EditLXDNetwork(req LXDNetworkEditRequest) error {
	if req.Name == "" {
		return fmt.Errorf("name is required")
	}
	if err := setLXDNetworkDescription(req.Name, req.Description); err != nil {
		return err
	}
	// Apply each config key.
	for k, v := range req.Config {
		if v == "" {
			if out, err := exec.Command("lxc", "network", "unset", req.Name, k).CombinedOutput(); err != nil {
				return fmt.Errorf("unset %s: %s", k, strings.TrimSpace(string(out)))
			}
		} else {
			if out, err := exec.Command("lxc", "network", "set", req.Name, k, v).CombinedOutput(); err != nil {
				return fmt.Errorf("set %s: %s", k, strings.TrimSpace(string(out)))
			}
		}
	}
	return nil
}

// DeleteLXDNetwork deletes an LXD network. If the network had a ZNAS-managed VLAN
// sub-interface, that stanza is also removed from /etc/network/interfaces.
func DeleteLXDNetwork(name string) error {
	// Get network detail first so we can check for VLAN external interfaces.
	net, err := GetLXDNetwork(name)
	if err != nil {
		return err
	}
	if net.VMCount > 0 {
		return fmt.Errorf("network is in use by %d instance(s)", net.VMCount)
	}

	externalIface := ""
	if net.Config != nil {
		externalIface = net.Config["bridge.external_interfaces"]
	}

	if out, err := exec.Command("lxc", "network", "delete", name).CombinedOutput(); err != nil {
		return fmt.Errorf("lxc network delete: %s", strings.TrimSpace(string(out)))
	}

	// Remove VLAN stanza if ZNAS created it.
	if externalIface != "" {
		existing, _ := os.ReadFile("/etc/network/interfaces")
		if strings.Contains(string(existing), "# znas-managed-vlan-start") &&
			strings.Contains(string(existing), " name="+externalIface+" ") {
			removeVLANInterfaceStanza(externalIface)
		}
	}
	return nil
}

// isVLANSubIface returns true if iface looks like a ZNAS-generated VLAN
// sub-interface of the form <parent>-v<digits>.
func isVLANSubIface(iface string) bool {
	idx := strings.LastIndex(iface, "-v")
	if idx < 0 {
		return false
	}
	rest := iface[idx+2:]
	if len(rest) == 0 {
		return false
	}
	for _, c := range rest {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// SetInterfaceMTU sets the MTU on a host network interface via `ip link set`.
func SetInterfaceMTU(iface string, mtu int) error {
	if mtu < 576 || mtu > 9000 {
		return fmt.Errorf("MTU must be between 576 and 9000")
	}
	out, err := exec.Command("sudo", "ip", "link", "set", iface, "mtu", fmt.Sprintf("%d", mtu)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip link set mtu: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// GetInterfaceMTU reads the current MTU for a host network interface.
func GetInterfaceMTU(iface string) (int, error) {
	data, err := os.ReadFile("/sys/class/net/" + iface + "/mtu")
	if err != nil {
		return 0, err
	}
	mtu, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", new(int))
	if err != nil || mtu == 0 {
		return 0, fmt.Errorf("could not parse MTU")
	}
	var v int
	fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &v)
	return v, nil
}

// ListPhysicalInterfaces returns non-virtual, non-loopback network interfaces
// suitable for use as the parent of a VLAN or plain external bridge.
func ListPhysicalInterfaces() ([]string, error) {
	data, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return nil, err
	}
	skip := []string{"lo", "lxd", "veth", "tap", "virbr", "docker", "br-", "vmbr0-"}
	var names []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if !strings.Contains(line, ":") {
			continue
		}
		iface := strings.SplitN(line, ":", 2)[0]
		iface = strings.TrimSpace(iface)
		if iface == "" {
			continue
		}
		bad := strings.Contains(iface, "-vlan") || isVLANSubIface(iface)
		if !bad {
			for _, pfx := range skip {
				if strings.HasPrefix(iface, pfx) {
					bad = true
					break
				}
			}
		}
		if !bad {
			names = append(names, iface)
		}
	}
	return names, nil
}
