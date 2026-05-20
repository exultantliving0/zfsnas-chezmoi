package system

// One-shot migration from netplan / systemd-networkd to ifupdown +
// /etc/network/interfaces. Triggered by the "Switch to systemd Networking"
// button on the VMs & Containers enable prereqs page (6.5.6+).
//
// Why we migrate AWAY from netplan rather than teaching the rest of the
// portal to speak netplan: the existing Step-3 of the enable flow already
// rewrites /etc/network/interfaces to convert physical NICs into bridge
// stanzas. Reimplementing the same surgery in netplan YAML doubles the
// surface and forces a renderer decision on every edit. Once VMs &
// Containers is on, all bridge edits go through ifupdown, so it's
// simplest to settle the host onto ifupdown at enable time.
//
// Scope (locked deliberately small for 6.5.6):
//   - Plain ethernet devices only.
//   - networkd renderer only — NetworkManager refuses (desktop install,
//     out of portal scope).
//   - No bonds, bridges, vlans, wifis in the netplan YAML — refuse with
//     a clear error so the user knows their setup needs manual
//     conversion.
//   - IPv4 only. IPv6 keeps SLAAC / kernel autoconf.
//
// On any failure mid-flight we roll back: re-enable netplan YAMLs,
// restart systemd-networkd, restore the original /etc/network/interfaces
// (or remove ours if there was none), and return an error.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// netplanScanResult is what a quick textual scan of /etc/netplan/*.yaml
// gives us: just enough to either refuse the migration with a clear note
// or hand off to the live-state-driven generator.
type netplanScanResult struct {
	Files       []string // absolute paths of *.yaml files
	Renderer    string   // "networkd" | "NetworkManager" | "" (default)
	Unsupported []string // names of constructs we won't migrate
	Devices     []string // physical-looking ethernet names found in the YAML
}

var (
	// Lines like "  enp5s0:" or "    enp5s0:" inside an "ethernets:" block.
	// We don't fully parse YAML (per CLAUDE.md, no new module deps) — we
	// just scan for top-level device-name keys under specific sections.
	netplanRendererRe   = regexp.MustCompile(`^\s*renderer:\s*([A-Za-z0-9_-]+)\s*$`)
	netplanSectionRe    = regexp.MustCompile(`^(\s*)(ethernets|bonds|bridges|vlans|wifis|tunnels|modems):\s*$`)
	netplanDeviceKeyRe  = regexp.MustCompile(`^(\s+)([A-Za-z0-9_.-]+):\s*$`)
)

// scanNetplanYAMLs reads every /etc/netplan/*.yaml and returns a coarse
// digest. It does NOT validate IP/routing/DNS — that comes from the live
// `ip -j` snapshot. Detected unsupported sections are returned in
// Unsupported so the caller can refuse with a clear message.
func scanNetplanYAMLs() (*netplanScanResult, error) {
	files, _ := filepath.Glob("/etc/netplan/*.yaml")
	if len(files) == 0 {
		return nil, fmt.Errorf("no /etc/netplan/*.yaml files found")
	}
	res := &netplanScanResult{Files: files}
	seenSection := map[string]bool{}
	for _, path := range files {
		// Netplan YAML is owned by root with 0600 since some releases.
		// `sudo cat` is in the existing wildcard-cat path (or bash:
		// the file browser feature already covers this); for migration
		// we prefer in-process read because it's simpler and we only
		// run from a user with sudo (verified upstream).
		data, err := readPossiblyRoot(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		var (
			currentSection      string
			currentSectionIndent int
		)
		scanner := bufio.NewScanner(strings.NewReader(string(data)))
		for scanner.Scan() {
			line := scanner.Text()
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if m := netplanRendererRe.FindStringSubmatch(line); m != nil {
				res.Renderer = m[1]
				continue
			}
			if m := netplanSectionRe.FindStringSubmatch(line); m != nil {
				currentSectionIndent = len(m[1])
				currentSection = m[2]
				if currentSection != "ethernets" {
					if !seenSection[currentSection] {
						res.Unsupported = append(res.Unsupported, currentSection)
						seenSection[currentSection] = true
					}
				}
				continue
			}
			// Inside an ethernets: block, top-level keys (one indent
			// level deeper than the section keyword) are device names.
			if currentSection == "ethernets" {
				if m := netplanDeviceKeyRe.FindStringSubmatch(line); m != nil && len(m[1]) > currentSectionIndent {
					name := m[2]
					// Skip nested keys like "match:", "addresses:", etc.
					// — those appear two indent levels deeper than the
					// section keyword. We accept exactly one level deeper.
					if len(m[1]) == currentSectionIndent+2 {
						res.Devices = append(res.Devices, name)
					}
				}
			}
		}
	}
	return res, nil
}

// readPossiblyRoot reads a file directly when the caller can, else via
// sudo cat. /etc/netplan files on Ubuntu 26.04 are 0600 root:root, so we
// fall back to sudo on permission errors.
func readPossiblyRoot(path string) ([]byte, error) {
	if data, err := os.ReadFile(path); err == nil {
		return data, nil
	} else if !os.IsPermission(err) {
		return nil, err
	}
	out, err := exec.Command("sudo", "/usr/bin/cat", path).Output()
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ipAddrLite is what we read from `ip -j addr` for the migration. We only
// touch a handful of fields, hence not reusing the bigger ipAddrIface struct
// from lxd_enable.go (different needs and we want this file self-contained).
type ipAddrLite struct {
	IfName   string `json:"ifname"`
	LinkType string `json:"link_type"`
	MTU      int    `json:"mtu"`
	Master   string `json:"master,omitempty"`
	AddrInfo []struct {
		Family    string `json:"family"`
		Local     string `json:"local"`
		PrefixLen int    `json:"prefixlen"`
		Dynamic   bool   `json:"dynamic,omitempty"`
		Scope     string `json:"scope"`
	} `json:"addr_info"`
}

type ipRouteLite struct {
	Dst     string `json:"dst"`
	Gateway string `json:"gateway,omitempty"`
	Dev     string `json:"dev"`
}

type liveDeviceSnapshot struct {
	Name    string
	IsDHCP  bool
	IPv4    string // "1.2.3.4/24"
	Gateway string // "" if not the default-route device
	MTU     int
	DNS     []string // global if device-specific not available
	// ClientID is the DHCP Option 61 byte string systemd-networkd was
	// presenting to the DHCP server, captured from /run/systemd/netif/leases.
	// Hex-encoded, lowercase, no separators (e.g. "0110666af8c2f0"). Empty
	// when the device wasn't on DHCP or the lease file couldn't be read.
	// Replayed into /etc/dhcpcd.conf so the host gets the SAME lease back
	// after the switch — without it, dhcpcd's default RFC-4361 DUID+IAID
	// Client-ID looks like a brand-new client to the DHCP server and the
	// server hands out a different IP.
	ClientID string
}

// snapshotLiveNet captures the host's current IPv4 view, keyed by interface.
// This is the source of truth for what we write into the new
// /etc/network/interfaces.
func snapshotLiveNet(devices []string) ([]liveDeviceSnapshot, error) {
	addrOut, err := exec.Command("ip", "-j", "addr").Output()
	if err != nil {
		return nil, fmt.Errorf("ip -j addr: %w", err)
	}
	var ifaces []ipAddrLite
	if err := json.Unmarshal(addrOut, &ifaces); err != nil {
		return nil, fmt.Errorf("parse ip addr: %w", err)
	}
	routeOut, err := exec.Command("ip", "-j", "route").Output()
	if err != nil {
		return nil, fmt.Errorf("ip -j route: %w", err)
	}
	var routes []ipRouteLite
	_ = json.Unmarshal(routeOut, &routes)
	gwByDev := map[string]string{}
	for _, r := range routes {
		if r.Dst == "default" && r.Gateway != "" {
			if _, exists := gwByDev[r.Dev]; !exists {
				gwByDev[r.Dev] = r.Gateway
			}
		}
	}
	dnsByDev, globalDNS := snapshotDNS()

	wanted := map[string]bool{}
	for _, d := range devices {
		wanted[d] = true
	}

	var out []liveDeviceSnapshot
	for _, iface := range ifaces {
		if !wanted[iface.IfName] {
			continue
		}
		if iface.LinkType != "ether" {
			continue
		}
		snap := liveDeviceSnapshot{Name: iface.IfName, MTU: iface.MTU}
		for _, a := range iface.AddrInfo {
			if a.Family != "inet" || a.Scope != "global" {
				continue
			}
			snap.IPv4 = fmt.Sprintf("%s/%d", a.Local, a.PrefixLen)
			snap.IsDHCP = a.Dynamic
			break
		}
		snap.Gateway = gwByDev[iface.IfName]
		if d := dnsByDev[iface.IfName]; len(d) > 0 {
			snap.DNS = d
		} else {
			snap.DNS = globalDNS
		}
		if snap.IsDHCP {
			snap.ClientID = readNetworkdClientID(iface.IfName)
		}
		out = append(out, snap)
	}
	return out, nil
}

// readNetworkdClientID parses /run/systemd/netif/leases/<ifindex> and returns
// the CLIENTID= line's value. Empty string on any read/parse failure — the
// migration then falls back to dhcpcd's default identifier (which means the
// DHCP server will likely hand out a different IP, but the migration still
// completes). Lease files are 644, owned by systemd-network — readable
// without sudo.
func readNetworkdClientID(ifname string) string {
	idxBytes, err := os.ReadFile("/sys/class/net/" + ifname + "/ifindex")
	if err != nil {
		return ""
	}
	ifindex := strings.TrimSpace(string(idxBytes))
	if ifindex == "" {
		return ""
	}
	data, err := os.ReadFile("/run/systemd/netif/leases/" + ifindex)
	if err != nil {
		return ""
	}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "CLIENTID=") {
			return strings.TrimPrefix(line, "CLIENTID=")
		}
	}
	return ""
}

// formatDhcpcdClientID converts a lowercase concatenated hex string like
// "0110666af8c2f0" into the colon-separated form dhcpcd's clientid directive
// expects: "01:10:66:6a:f8:c2:f0". Returns "" if the input isn't an even
// number of hex chars.
func formatDhcpcdClientID(hex string) string {
	if len(hex) == 0 || len(hex)%2 != 0 {
		return ""
	}
	var b strings.Builder
	for i := 0; i < len(hex); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(hex[i : i+2])
	}
	return b.String()
}

// dhcpcdConfigBlockMarkers wrap the migration-managed section of
// /etc/dhcpcd.conf so we can find and replace it on subsequent runs.
const (
	dhcpcdMarkerStart = "# >>> ZNAS netplan→ifupdown migration — clientid pinning >>>"
	dhcpcdMarkerEnd   = "# <<< ZNAS netplan→ifupdown migration — clientid pinning <<<"
)

// pinDhcpcdClientIDs appends per-interface `clientid` directives to
// /etc/dhcpcd.conf for every DHCP device whose Client-ID we captured from
// systemd-networkd. The block is bounded by start/end marker comments so
// re-running the migration replaces it cleanly. Returns nil when no
// devices have a captured Client-ID (no-op).
func pinDhcpcdClientIDs(snap []liveDeviceSnapshot, log func(string)) error {
	const path = "/etc/dhcpcd.conf"
	var block strings.Builder
	block.WriteString(dhcpcdMarkerStart + "\n")
	wrote := 0
	for _, d := range snap {
		if !d.IsDHCP || d.ClientID == "" {
			continue
		}
		cid := formatDhcpcdClientID(d.ClientID)
		if cid == "" {
			continue
		}
		fmt.Fprintf(&block, "interface %s\n    clientid %s\n", d.Name, cid)
		wrote++
		log(fmt.Sprintf("    %s ← clientid %s (matches systemd-networkd's lease ID)", d.Name, cid))
	}
	block.WriteString(dhcpcdMarkerEnd + "\n")
	if wrote == 0 {
		return nil
	}

	existing, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	stripped := stripBlockBetweenMarkers(string(existing), dhcpcdMarkerStart, dhcpcdMarkerEnd)
	stripped = strings.TrimRight(stripped, "\n") + "\n\n"
	out := stripped + block.String()

	if err := writeRoot(path, []byte(out), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// stripBlockBetweenMarkers removes everything from the start marker to the
// end marker (inclusive). Returns the input unchanged if the markers aren't
// found in pair order.
func stripBlockBetweenMarkers(s, start, end string) string {
	a := strings.Index(s, start)
	if a < 0 {
		return s
	}
	b := strings.Index(s[a:], end)
	if b < 0 {
		return s
	}
	tail := s[a+b+len(end):]
	tail = strings.TrimLeft(tail, "\n")
	return strings.TrimRight(s[:a], "\n") + "\n" + tail
}

// snapshotDNS returns per-interface DNS servers and a global fallback,
// trying three sources in order so the migration captures DNS on every
// realistic host shape:
//
//  1. `resolvectl dns` — the primary path on hosts that have
//     systemd-resolved (Ubuntu defaults, most netplan installs).
//  2. `/etc/resolv.conf` `nameserver` lines — works on any host that
//     currently resolves DNS, including minimal Debian/Ubuntu installs
//     that ship without systemd-resolved at all (where `resolvectl`
//     itself is "command not found"). Confirmed root cause of a real
//     production failure in v6.5.6 — without this
//     fallback the post-migration `/etc/network/interfaces` had no
//     `dns-nameservers` line and `/etc/resolv.conf` lost its servers.
//  3. `/etc/netplan/*.yaml` `nameservers.addresses` — last-resort
//     capture for hosts where DNS is set in netplan but never made it
//     into a live resolver (e.g. a freshly edited YAML that hasn't
//     been applied yet, or a non-default renderer).
//
// Per-link and global DNS are accumulated independently across the three
// sources: a host with `/etc/resolv.conf` populated but no resolvectl
// still gets its DNS into the global list, and a host with per-interface
// DNS in netplan YAML still gets that into the per-interface map even
// when the global list comes from /etc/resolv.conf.
func snapshotDNS() (map[string][]string, []string) {
	per := map[string][]string{}
	var global []string

	// 1. resolvectl (best source — knows about per-link config).
	if out, err := exec.Command("resolvectl", "dns", "--no-pager").Output(); err == nil {
		linkRe := regexp.MustCompile(`^Link\s+\d+\s+\(([^)]+)\):\s*(.*)$`)
		globalRe := regexp.MustCompile(`^Global:\s*(.*)$`)
		scanner := bufio.NewScanner(strings.NewReader(string(out)))
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if m := linkRe.FindStringSubmatch(line); m != nil {
				per[m[1]] = strings.Fields(m[2])
				continue
			}
			if m := globalRe.FindStringSubmatch(line); m != nil {
				global = strings.Fields(m[1])
			}
		}
	}

	// 2. /etc/resolv.conf — only fills the global slot if resolvectl
	// didn't already populate it. The file may be a regular file (the
	// hand-edited or migrated form) or a symlink to systemd-resolved's
	// stub: os.Open follows the symlink either way.
	if len(global) == 0 {
		global = readResolvConfNameservers("/etc/resolv.conf")
	}

	// 3. /etc/netplan/*.yaml `nameservers.addresses`. Merged into the
	// per-interface map for any device that still has nothing, and
	// promoted to the global list if global is also empty.
	yamlPer := readNetplanNameservers()
	for ifname, dns := range yamlPer {
		if len(per[ifname]) == 0 {
			per[ifname] = dns
		}
	}
	if len(global) == 0 {
		seen := map[string]bool{}
		for _, dns := range yamlPer {
			for _, s := range dns {
				if seen[s] {
					continue
				}
				seen[s] = true
				global = append(global, s)
			}
		}
	}

	return per, global
}

// readResolvConfNameservers scans a resolv.conf-shaped file for
// `nameserver <ip>` lines. Empty result on any read or parse error —
// callers treat empty as "fall through to next source".
func readResolvConfNameservers(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		// Some libresolv-conf forms include a comment after the IP;
		// take the first whitespace-separated token only.
		ip := fields[1]
		if seen[ip] {
			continue
		}
		seen[ip] = true
		out = append(out, ip)
	}
	return out
}

// readNetplanNameservers parses every /etc/netplan/*.yaml looking for
// per-interface `nameservers: { addresses: [...] }` blocks under an
// `ethernets:` section. We stay with the same indent-aware textual
// scan used by scanNetplanYAMLs (per CLAUDE.md, no new module deps for
// a real YAML parser). Result keys are interface names.
//
// Two YAML shapes are accepted:
//
//	ethernets:
//	  enp5s0:
//	    nameservers:
//	      addresses: [10.0.0.1, 10.0.0.2]
//
//	ethernets:
//	  enp5s0:
//	    nameservers:
//	      addresses:
//	        - 10.0.0.1
//	        - 10.0.0.2
//
// Anything we can't parse cleanly is silently skipped.
func readNetplanNameservers() map[string][]string {
	out := map[string][]string{}
	files, _ := filepath.Glob("/etc/netplan/*.yaml")
	for _, path := range files {
		data, err := readPossiblyRoot(path)
		if err != nil {
			continue
		}
		parseNetplanNameserversInto(string(data), out)
	}
	return out
}

// parseNetplanNameserversInto walks a single YAML doc's lines and merges
// any per-interface DNS it finds into `dst`. Pulled out so a unit test
// can pin the parser without touching /etc/netplan.
func parseNetplanNameserversInto(yaml string, dst map[string][]string) {
	var (
		inEthernets        bool
		ethernetsIndent    int
		ifname             string
		ifnameIndent       int
		inNameservers      bool
		nameserversIndent  int
		inAddressesList    bool
		addressesIndent    int
	)
	inlineListRe := regexp.MustCompile(`^\s*addresses:\s*\[([^\]]*)\]\s*$`)
	listItemRe   := regexp.MustCompile(`^\s*-\s*([0-9A-Fa-f.:]+)\s*$`)

	for _, line := range strings.Split(yaml, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))

		// Section change: any non-deeper-than-context key resets the
		// nested state we were tracking.
		if inAddressesList && indent <= addressesIndent {
			inAddressesList = false
		}
		if inNameservers && indent <= nameserversIndent {
			inNameservers = false
		}
		if ifname != "" && indent <= ifnameIndent {
			ifname = ""
		}
		if inEthernets && indent <= ethernetsIndent && !strings.HasPrefix(trimmed, "ethernets:") {
			inEthernets = false
		}

		if m := netplanSectionRe.FindStringSubmatch(line); m != nil {
			if m[2] == "ethernets" {
				inEthernets = true
				ethernetsIndent = len(m[1])
			}
			continue
		}
		if !inEthernets {
			continue
		}

		// Device key inside ethernets:
		if m := netplanDeviceKeyRe.FindStringSubmatch(line); m != nil && len(m[1]) == ethernetsIndent+2 {
			ifname = m[2]
			ifnameIndent = len(m[1])
			inNameservers = false
			inAddressesList = false
			continue
		}
		if ifname == "" {
			continue
		}

		// `nameservers:` opens a sub-block.
		if strings.HasPrefix(trimmed, "nameservers:") && indent > ifnameIndent {
			inNameservers = true
			nameserversIndent = indent
			continue
		}
		if !inNameservers {
			continue
		}

		// Inline form: addresses: [a, b].
		if m := inlineListRe.FindStringSubmatch(line); m != nil {
			for _, s := range strings.Split(m[1], ",") {
				s = strings.TrimSpace(s)
				s = strings.Trim(s, "\"'")
				if s != "" {
					dst[ifname] = append(dst[ifname], s)
				}
			}
			continue
		}
		// Block form: addresses:\n  - x\n  - y
		if strings.HasPrefix(trimmed, "addresses:") {
			inAddressesList = true
			addressesIndent = indent
			continue
		}
		if inAddressesList {
			if m := listItemRe.FindStringSubmatch(line); m != nil {
				dst[ifname] = append(dst[ifname], m[1])
			}
		}
	}
}

// renderInterfacesFile composes the /etc/network/interfaces text from the
// live snapshot. Always emits the lo loopback. Per-NIC stanza picks
// inet dhcp vs inet static based on whether the live address is dynamic.
func renderInterfacesFile(devices []liveDeviceSnapshot) string {
	var b strings.Builder
	b.WriteString("# /etc/network/interfaces — written by ZNAS netplan→ifupdown migration.\n")
	b.WriteString("# Source of truth was the live `ip -j addr` / `ip -j route` /\n")
	b.WriteString("# `resolvectl dns` output at migration time.\n\n")
	b.WriteString("auto lo\niface lo inet loopback\n\n")
	for _, d := range devices {
		b.WriteString(fmt.Sprintf("auto %s\n", d.Name))
		if d.IsDHCP || d.IPv4 == "" {
			// DHCP path: deliberately omit `dns-nameservers`. The
			// installed dhcpcd exit-hook pushes the DHCP-supplied
			// DNS into systemd-resolved via `resolvectl`, so no
			// static value should be baked into the interfaces file
			// (it would override the live DHCP answer).
			b.WriteString(fmt.Sprintf("iface %s inet dhcp\n", d.Name))
		} else {
			b.WriteString(fmt.Sprintf("iface %s inet static\n", d.Name))
			b.WriteString(fmt.Sprintf("    address %s\n", d.IPv4))
			if d.Gateway != "" {
				b.WriteString(fmt.Sprintf("    gateway %s\n", d.Gateway))
			}
			// Static path: the user's chosen DNS is part of the
			// static config and lives in the interfaces file. Step 3
			// of the enable flow preserves it onto the bridge stanza.
			if len(d.DNS) > 0 {
				b.WriteString(fmt.Sprintf("    dns-nameservers %s\n", strings.Join(d.DNS, " ")))
			}
		}
		if d.MTU > 0 && d.MTU != 1500 {
			b.WriteString(fmt.Sprintf("    mtu %d\n", d.MTU))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// MigrateNetplanToIfupdown runs the full migration and streams progress.
// stream is called with single short lines (newline-free). On any error
// the function rolls back what it can and returns the error; the caller
// (HTTP handler) just relays the error to the user.
func MigrateNetplanToIfupdown(stream func(string)) error {
	log := func(s string) {
		if stream != nil {
			stream(s)
		}
	}

	log("Scanning /etc/netplan/*.yaml…")
	scan, err := scanNetplanYAMLs()
	if err != nil {
		return err
	}
	log(fmt.Sprintf("  Found %d file(s); renderer=%q; ethernets=%v",
		len(scan.Files), scan.Renderer, scan.Devices))

	if strings.EqualFold(scan.Renderer, "NetworkManager") {
		return fmt.Errorf("netplan renderer is NetworkManager — automatic migration only supports the networkd renderer. Convert to ifupdown manually")
	}
	if len(scan.Unsupported) > 0 {
		return fmt.Errorf("netplan contains unsupported sections (%s); migrate manually before enabling VMs & Containers",
			strings.Join(scan.Unsupported, ", "))
	}
	if len(scan.Devices) == 0 {
		return fmt.Errorf("no ethernet devices found in netplan YAML")
	}

	log("Snapshotting live IP / route / DNS state…")
	snap, err := snapshotLiveNet(scan.Devices)
	if err != nil {
		return err
	}
	for _, d := range snap {
		mode := "dhcp"
		if !d.IsDHCP && d.IPv4 != "" {
			mode = "static " + d.IPv4
			if d.Gateway != "" {
				mode += " gw " + d.Gateway
			}
		}
		log(fmt.Sprintf("  %s — %s, mtu=%d, dns=%v", d.Name, mode, d.MTU, d.DNS))
	}

	// Step 1: install ifupdown if missing.
	if !pkgInstalled("ifupdown") {
		log("Installing ifupdown package…")
		if out, err := exec.Command("sudo", "/usr/bin/apt-get", "install", "-y", "ifupdown").CombinedOutput(); err != nil {
			return fmt.Errorf("apt-get install ifupdown: %s", strings.TrimSpace(string(out)))
		}
	} else {
		log("ifupdown already installed.")
	}

	// Step 1b: pin dhcpcd's Client-Identifier to whatever systemd-networkd
	// was using, so the DHCP server hands the same lease back. Best-effort —
	// a failure here doesn't abort the migration; the host still gets an
	// IP, just possibly a different one.
	log("Pinning dhcpcd Client-ID per interface to match systemd-networkd…")
	if err := pinDhcpcdClientIDs(snap, log); err != nil {
		log("  ⚠ Could not pin dhcpcd clientid: " + err.Error() + " (continuing — IP may change)")
	}

	// Step 2: backup + write /etc/network/interfaces.
	newContent := renderInterfacesFile(snap)
	log("New /etc/network/interfaces:\n" + indent(newContent, "    | "))

	const targetPath = "/etc/network/interfaces"
	var backupPath string
	if existing, err := os.ReadFile(targetPath); err == nil {
		backupPath = fmt.Sprintf("%s.pre-znas-%d", targetPath, time.Now().Unix())
		log("Backing up existing " + targetPath + " → " + backupPath)
		if err := writeRoot(backupPath, existing, 0o644); err != nil {
			return fmt.Errorf("backup %s: %w", targetPath, err)
		}
	}

	log("Writing " + targetPath + "…")
	if err := writeRoot(targetPath, []byte(newContent), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", targetPath, err)
	}

	// Step 3: stop + disable systemd-networkd AND its activation sockets.
	// On Ubuntu 26.04 there are three socket units that can re-trigger
	// the service even after we stop it (varlink / resolve-hook / the
	// main netlink socket). We disable all of them; otherwise the
	// service comes back ~instantly after stop and reapplies the
	// netplan-generated /run/systemd/network/*.network configs we're
	// trying to walk away from. We tolerate "not loaded" / "does not
	// exist" so older releases without one or another socket don't
	// abort the migration.
	log("Disabling systemd-networkd (service + activation sockets)…")
	networkdUnits := []string{
		"systemd-networkd-resolve-hook.socket",
		"systemd-networkd-varlink.socket",
		"systemd-networkd.socket",
		"systemd-networkd.service",
	}
	for _, u := range networkdUnits {
		for _, action := range []string{"disable", "stop"} {
			out, err := exec.Command("sudo", "/usr/bin/systemctl", action, u).CombinedOutput()
			if err == nil {
				continue
			}
			msg := strings.TrimSpace(string(out))
			if !strings.Contains(msg, "not loaded") &&
				!strings.Contains(msg, "does not exist") &&
				!strings.Contains(msg, "Failed to disable") {
				log(fmt.Sprintf("  systemctl %s %s: %s (continuing)", action, u, msg))
			}
		}
	}

	// Remove the netplan-generated systemd-networkd runtime configs.
	// These live on tmpfs (/run) and would normally vanish on reboot,
	// but until then they're what makes a re-spawned systemd-networkd
	// instantly start managing our NICs again.
	log("Removing leftover /run/systemd/network/*.network files…")
	if matches, _ := filepath.Glob("/run/systemd/network/*.network"); len(matches) > 0 {
		args := append([]string{"/usr/bin/rm", "-f"}, matches...)
		if out, err := exec.Command("sudo", args...).CombinedOutput(); err != nil {
			log("  rm: " + strings.TrimSpace(string(out)) + " (continuing)")
		}
	}

	// Step 4: rename netplan YAMLs out of the way.
	for _, p := range scan.Files {
		dst := p + ".znas-disabled"
		log("Renaming " + p + " → " + dst)
		if out, err := exec.Command("sudo", "/usr/bin/mv", p, dst).CombinedOutput(); err != nil {
			// Hard failure — without this rename, a future netplan apply
			// would clobber our config.
			rollbackInterfaces(targetPath, backupPath)
			rollbackNetworkd()
			return fmt.Errorf("mv %s: %s", p, strings.TrimSpace(string(out)))
		}
	}

	// Step 5: drop a cloud-init disable file (if cloud-init is installed)
	// so the next reboot doesn't regenerate /etc/netplan/50-cloud-init.yaml.
	if _, err := os.Stat("/etc/cloud/cloud.cfg.d"); err == nil {
		const disableFile = "/etc/cloud/cloud.cfg.d/99-znas-disable-network-config.cfg"
		log("Disabling cloud-init network management → " + disableFile)
		body := "# Written by ZNAS during netplan→ifupdown migration.\nnetwork: {config: disabled}\n"
		if err := writeRoot(disableFile, []byte(body), 0o644); err != nil {
			log("  ⚠ Could not write " + disableFile + ": " + err.Error())
		}
	} else {
		log("cloud-init not present; skipping its network-disable drop-in.")
	}

	// Step 5b: keep DNS dynamic for DHCP interfaces.
	//
	// Approach: leave systemd-resolved running and let it own /etc/resolv.conf
	// (the standard Ubuntu 26.04 stub-symlink). Install a small dhcpcd
	// exit-hook that, on every BOUND/RENEW/REBIND, calls `resolvectl dns
	// <iface> <ip…>` so resolved learns the DHCP-supplied DNS for that
	// interface. Static interfaces aren't touched here — for those, the
	// user's `dns-nameservers` line is the authoritative config and the
	// hook (which also calls resolvectl on STATIC interfaces with
	// dns-nameservers via ifupdown's resolvectl integration) is unnecessary.
	//
	// We never rewrite /etc/resolv.conf as a regular file. If a previous
	// migration of ours did, restore the stub symlink so resolved owns it
	// again.
	log("Installing dhcpcd → systemd-resolved exit-hook…")
	if err := installDhcpcdResolvedHook(); err != nil {
		log("  ⚠ Could not install dhcpcd hook: " + err.Error() + " (DHCP-supplied DNS may not propagate)")
	} else {
		log("  ✓ /etc/dhcpcd.exit-hook in place — DHCP DNS will be pushed to systemd-resolved per interface")
	}

	// Pick the right /etc/resolv.conf shape based on the user's intent:
	//
	//   • All DHCP → restore the systemd-resolved stub symlink. The hook
	//     above feeds resolved with whatever DNS the DHCP server hands
	//     back, dynamically.
	//
	//   • Any static interface with dns-nameservers → write /etc/resolv.conf
	//     as a regular file containing those static DNS servers. The
	//     user opted for static, so their DNS is part of that fixed
	//     config — it lives both in /etc/network/interfaces and in
	//     /etc/resolv.conf, exactly as the user expects.
	if dns := collectStaticDNS(snap); len(dns) > 0 {
		log(fmt.Sprintf("Writing /etc/resolv.conf with static DNS %v (from static-IP interface(s))…", dns))
		if err := writeStaticResolvConf(dns); err != nil {
			log("  ⚠ Could not write static /etc/resolv.conf: " + err.Error())
		}
	} else {
		if err := ensureResolvConfStubSymlink(log); err != nil {
			log("  ⚠ Could not restore /etc/resolv.conf symlink: " + err.Error())
		}
	}

	// Step 6: enable + start ifupdown's networking.service. This is the
	// "may briefly drop the link" moment.
	log("Enabling networking.service (ifupdown)…")
	if out, err := exec.Command("sudo", "/usr/bin/systemctl", "enable", "networking").CombinedOutput(); err != nil {
		log("  systemctl enable networking: " + strings.TrimSpace(string(out)))
	}

	// Flush each managed interface's IPv4 addresses + routes before
	// `ifup -a` runs. Without this, the addresses configured by netplan
	// (and not always reaped when systemd-networkd stops, especially for
	// static configs) collide with `ip addr add` inside the ifup hooks
	// and `networking.service` exits non-zero — even though the live
	// state is correct. The flush is brief (sub-second), the new
	// addresses come back via `ifup -a` immediately after, and the
	// dhcpcd Client-ID pinning means DHCP returns the same lease.
	log("Flushing managed interfaces' IPv4 state to avoid `ip addr add: File exists`…")
	for _, d := range snap {
		exec.Command("sudo", "/usr/bin/ip", "addr", "flush", "dev", d.Name, "scope", "global").Run()  //nolint:errcheck
		exec.Command("sudo", "/usr/bin/ip", "route", "flush", "dev", d.Name, "scope", "global").Run() //nolint:errcheck
	}

	log("Starting networking.service — your shell or HTTPS session may briefly hiccup…")
	if out, err := exec.Command("sudo", "/usr/bin/systemctl", "start", "networking").CombinedOutput(); err != nil {
		// On many Ubuntu installs `systemctl start networking` returns
		// non-zero because some interface failed (loopback already up,
		// etc.) but the device we care about IS up. Verify before we
		// roll back.
		log("  systemctl start networking returned an error: " + strings.TrimSpace(string(out)))
	}

	// Step 7: verify the managed devices still hold an IPv4 address.
	// Give ifupdown's dhclient a moment to finish.
	time.Sleep(3 * time.Second)
	bad := devicesMissingIPv4(scan.Devices)
	if len(bad) > 0 {
		log("Verification failed — interface(s) lost their IPv4: " + strings.Join(bad, ", "))
		log("Rolling back…")
		rollbackInterfaces(targetPath, backupPath)
		// Restore the netplan YAMLs.
		for _, p := range scan.Files {
			exec.Command("sudo", "/usr/bin/mv", p+".znas-disabled", p).Run() //nolint:errcheck
		}
		rollbackNetworkd()
		return fmt.Errorf("ifupdown could not bring up %s — original network config restored",
			strings.Join(bad, ", "))
	}

	log("✓ Migration complete. /etc/network/interfaces is now authoritative.")
	return nil
}

// devicesMissingIPv4 returns the subset of names that no longer have a
// global IPv4 address in `ip -j addr`.
func devicesMissingIPv4(names []string) []string {
	out, err := exec.Command("ip", "-j", "addr").Output()
	if err != nil {
		return names
	}
	var ifaces []ipAddrLite
	if err := json.Unmarshal(out, &ifaces); err != nil {
		return names
	}
	hasIP := map[string]bool{}
	for _, iface := range ifaces {
		for _, a := range iface.AddrInfo {
			if a.Family == "inet" && a.Scope == "global" {
				hasIP[iface.IfName] = true
				break
			}
		}
	}
	var missing []string
	for _, n := range names {
		if !hasIP[n] {
			missing = append(missing, n)
		}
	}
	return missing
}

// pkgInstalled returns true if dpkg-query reports the named .deb is installed.
func pkgInstalled(name string) bool {
	out, err := exec.Command("dpkg-query", "-W", "-f=${Status}", name).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "install ok installed")
}

// writeRoot writes data to path, using direct I/O when running as root and
// `sudo tee` otherwise. Matches writeInterfacesFile in lxd_enable.go: tee
// writes the file with default 0644 mode; we don't chmod afterwards to
// keep the sudoers surface narrow (only `tee <path>` needed).
func writeRoot(path string, data []byte, _ os.FileMode) error {
	if os.Getuid() == 0 {
		return os.WriteFile(path, data, 0o644)
	}
	cmd := exec.Command("sudo", "/usr/bin/tee", path)
	cmd.Stdin = strings.NewReader(string(data))
	cmd.Stdout = nil
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}

// dhcpcdResolvedHookPath is where the dhcpcd→systemd-resolved bridge
// script lives. dhcpcd runs this after every lease event with a populated
// $reason and $new_domain_name_servers / $new_domain_search.
const dhcpcdResolvedHookPath = "/etc/dhcpcd.exit-hook"

// dhcpcdResolvedHookBody is the entire exit-hook content. Idempotent —
// dhcpcd may invoke the hook multiple times per second during fast
// renewals; resolvectl handles the no-op case fine.
const dhcpcdResolvedHookBody = `#!/bin/sh
# Managed by ZNAS netplan→ifupdown migration.
# Push DHCP-supplied DNS into systemd-resolved on every lease event so
# /etc/resolv.conf (the systemd-resolved stub) sees real upstream DNS
# without us baking a static dns-nameservers line into the interfaces
# file. Removing this hook returns to dhcpcd's default resolv.conf
# behaviour.

case "$reason" in
    BOUND|RENEW|REBIND|REBOOT|INFORM)
        if [ -n "$new_domain_name_servers" ]; then
            /usr/bin/resolvectl dns "$interface" $new_domain_name_servers 2>/dev/null || true
        fi
        if [ -n "$new_domain_search" ] || [ -n "$new_domain_name" ]; then
            /usr/bin/resolvectl domain "$interface" $new_domain_search $new_domain_name 2>/dev/null || true
        fi
        ;;
    EXPIRE|FAIL|IPV4LL|NOCARRIER|STOP|RELEASE)
        /usr/bin/resolvectl revert "$interface" 2>/dev/null || true
        ;;
esac
`

// installDhcpcdResolvedHook writes the dhcpcd exit-hook and makes it
// executable. Idempotent — re-running the migration overwrites the file
// with the same content (so a future hook update lands on next migration).
func installDhcpcdResolvedHook() error {
	if err := writeRoot(dhcpcdResolvedHookPath, []byte(dhcpcdResolvedHookBody), 0o755); err != nil {
		return err
	}
	if out, err := exec.Command("sudo", "/usr/bin/chmod", "0755", dhcpcdResolvedHookPath).CombinedOutput(); err != nil {
		return fmt.Errorf("chmod 0755 %s: %s", dhcpcdResolvedHookPath, strings.TrimSpace(string(out)))
	}
	return nil
}

// collectStaticDNS returns the deduped DNS servers across every STATIC
// device that has dns-nameservers configured. DHCP devices are skipped
// — their DNS comes dynamically via the dhcpcd → systemd-resolved hook.
// Returns nil when no static device has DNS, which signals the caller
// to fall back to the systemd-resolved stub symlink.
func collectStaticDNS(devices []liveDeviceSnapshot) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range devices {
		if d.IsDHCP {
			continue
		}
		for _, s := range d.DNS {
			if seen[s] {
				continue
			}
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// writeStaticResolvConf replaces /etc/resolv.conf with a regular file
// containing the given nameservers — used when at least one interface is
// static (the user opted out of dynamic DNS for that NIC, so the resolver
// config should be static too). If /etc/resolv.conf is currently a
// symlink (Ubuntu/Debian default → systemd-resolved stub), it is removed
// first; otherwise `tee` would write through into the resolved-managed
// run-time file, which is the opposite of what we want.
func writeStaticResolvConf(dns []string) error {
	const path = "/etc/resolv.conf"
	var b strings.Builder
	b.WriteString("# Written by ZNAS netplan→ifupdown migration.\n")
	b.WriteString("# Static DNS preserved from your static-IP interface(s);\n")
	b.WriteString("# replaces the systemd-resolved stub symlink so the user's\n")
	b.WriteString("# explicit dns-nameservers config is what gets queried.\n")
	for _, s := range dns {
		b.WriteString("nameserver " + s + "\n")
	}
	exec.Command("sudo", "/usr/bin/rm", "-f", path).Run() //nolint:errcheck
	return writeRoot(path, []byte(b.String()), 0o644)
}

// ensureResolvConfStubSymlink restores /etc/resolv.conf to point at
// /run/systemd/resolve/stub-resolv.conf (the standard Ubuntu / Debian
// systemd-resolved layout). No-op when the symlink already targets the
// stub. If the file is a regular file (e.g. left behind by an older ZNAS
// migration), it's removed first.
func ensureResolvConfStubSymlink(log func(string)) error {
	const target = "/run/systemd/resolve/stub-resolv.conf"
	const path = "/etc/resolv.conf"

	if cur, err := os.Readlink(path); err == nil && cur == target {
		return nil
	}
	log("Restoring /etc/resolv.conf → " + target + " (systemd-resolved stub)…")
	exec.Command("sudo", "/usr/bin/rm", "-f", path).Run() //nolint:errcheck
	if out, err := exec.Command("sudo", "/usr/bin/ln", "-sf", target, path).CombinedOutput(); err != nil {
		return fmt.Errorf("ln -sf: %s", strings.TrimSpace(string(out)))
	}
	// systemd-resolved is left running through the migration; ensure it's
	// enabled so the stub keeps resolving on the next reboot.
	exec.Command("sudo", "/usr/bin/systemctl", "enable", "systemd-resolved").Run() //nolint:errcheck
	exec.Command("sudo", "/usr/bin/systemctl", "start", "systemd-resolved").Run()  //nolint:errcheck
	return nil
}

func indent(s, prefix string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString(prefix)
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}

// rollbackInterfaces restores the /etc/network/interfaces backup if there
// was one, or removes the file we wrote if there wasn't.
func rollbackInterfaces(targetPath, backupPath string) {
	if backupPath == "" {
		exec.Command("sudo", "/usr/bin/rm", "-f", targetPath).Run() //nolint:errcheck
		return
	}
	if data, err := os.ReadFile(backupPath); err == nil {
		_ = writeRoot(targetPath, data, 0o644)
	}
}

// rollbackNetworkd re-enables systemd-networkd and its activation
// sockets. Best-effort. Mirrors the units we disabled in the forward
// pass so a rollback leaves systemd-networkd in the same state we
// found it.
func rollbackNetworkd() {
	for _, u := range []string{
		"systemd-networkd-resolve-hook.socket",
		"systemd-networkd-varlink.socket",
		"systemd-networkd.socket",
		"systemd-networkd.service",
	} {
		exec.Command("sudo", "/usr/bin/systemctl", "enable", u).Run() //nolint:errcheck
		exec.Command("sudo", "/usr/bin/systemctl", "start", u).Run()  //nolint:errcheck
	}
}
