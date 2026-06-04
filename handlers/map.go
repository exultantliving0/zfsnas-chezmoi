package handlers

import (
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"zfsnas/internal/config"
	"zfsnas/internal/rrd"
	"zfsnas/system"
)

// ─────────────────────────────────────────────────────────────────────────────
// Live Storage Map (v6.6.3)
//
// GET /api/map/topology returns one self-contained topology document describing
// the whole server bottom-to-top: physical disks → ZFS pool → datasets/zvols →
// consumers (SMB/NFS/iSCSI/S3 shares + VMs/containers) → connected remote
// systems. The frontend (Capacity Trend → "Map" tab) renders it as an animated
// SVG HUD and polls this endpoint every ~3s.
//
// This handler is an *aggregation layer*: every value comes from functions that
// already exist in system/. The only live, per-poll work is GetAllPools, the
// dataset/zvol listings, and the session lookups — all of which the SMB/NFS/
// pool tabs already invoke at comparable cadence. Per-disk IOPS and CPU/mem/net
// come from background pollers (already running), and static disk metadata
// (model/serial/size) is cached for 60s so the SMART-heavy ListDisks call does
// not run on every poll.
// ─────────────────────────────────────────────────────────────────────────────

type mapServer struct {
	Hostname    string  `json:"hostname"`
	CPUPct      float64 `json:"cpu_pct"`
	MemPct      float64 `json:"mem_pct"`
	NetRxMbps   float64 `json:"net_rx_mbps"`
	NetTxMbps   float64 `json:"net_tx_mbps"`
	VirtEnabled bool    `json:"virt_enabled"`
}

type mapPool struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	State     string  `json:"state"`
	Used      uint64  `json:"used"`
	Usable    uint64  `json:"usable"`
	ARCPct    float64 `json:"arc_pct"`
	ReadKBps  float64 `json:"read_kbps"`
	WriteKBps float64 `json:"write_kbps"`
}

type mapDisk struct {
	ID        string  `json:"id"`
	PoolID    string  `json:"pool_id"`
	Dev       string  `json:"dev"`
	Model     string  `json:"model,omitempty"`
	Serial    string  `json:"serial,omitempty"`
	SizeStr   string  `json:"size_str,omitempty"`
	DiskType  string  `json:"disk_type,omitempty"`
	Role      string  `json:"role,omitempty"`
	Status    string  `json:"status"`
	Present   bool    `json:"present"`
	ReadKBps  float64 `json:"read_kbps"`
	WriteKBps float64 `json:"write_kbps"`
	BusyPct   float64 `json:"busy_pct"`
}

type mapDataset struct {
	ID        string `json:"id"`
	PoolID    string `json:"pool_id"`
	Kind      string `json:"kind"` // "filesystem" | "zvol"
	Name      string `json:"name"`
	Used      uint64 `json:"used"`
	Avail     uint64 `json:"avail"`
	Encrypted bool   `json:"encrypted"`
}

type mapConsumer struct {
	ID         string   `json:"id"`
	Type       string   `json:"type"` // smb|nfs|iscsi|s3|vm|container
	Name       string   `json:"name"`
	DatasetIDs []string `json:"dataset_ids"`     // backing dataset(s)/zvol(s); a VM links to each virtual-disk zvol
	State      string   `json:"state,omitempty"` // vm/container run state
	Clients    int      `json:"clients"`         // number of connected remotes
	RateKBps   *float64 `json:"rate_kbps"`       // null ⇒ unknown (no label, flow up)
	IP         string   `json:"ip,omitempty"`    // vm/container guest IPv4 (used to fold in self-mounts)
	// ClientOf lists share consumer IDs this VM/container mounts as a client
	// (its guest IP matched the SMB/NFS session). Drawn as a lateral up-then-down
	// link to the share instead of a duplicate remote box at the top.
	ClientOf []string `json:"client_of,omitempty"`
}

type mapRemote struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`        // hostname/FQDN when known, else the IP
	IP          string   `json:"ip,omitempty"` // shown on its own row when a hostname is the label
	ConsumerIDs []string `json:"consumer_ids"` // a client may use several shares → one box, many links
	Kind        string   `json:"kind"`         // "client" | "peer"
	Dir         string   `json:"dir"`          // up|down|both|unknown
}

type mapTopology struct {
	Server    mapServer     `json:"server"`
	Pools     []mapPool     `json:"pools"`
	Disks     []mapDisk     `json:"disks"`
	Datasets  []mapDataset  `json:"datasets"`
	Consumers []mapConsumer `json:"consumers"`
	Remotes   []mapRemote   `json:"remotes"`
	TS        int64         `json:"ts"`
}

// HandleMapTopology builds the live topology document. Read-only; gated like
// Capacity Trend (RequireAuth) in the router.
// GET /api/map/topology
func HandleMapTopology(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		top := buildMapTopology(appCfg)
		jsonOK(w, top)
	}
}

var reInstanceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)

// HandleMapInstanceMetrics returns live CPU/memory/filesystem usage for one
// VM or container, for the Map hover popup.
// GET /api/map/instance-metrics?name=<instance>
func HandleMapInstanceMetrics(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if !reInstanceName.MatchString(name) {
		jsonErr(w, http.StatusBadRequest, "invalid instance name")
		return
	}
	if !system.LXDAvailable() {
		jsonErr(w, http.StatusServiceUnavailable, "virtualization not available")
		return
	}
	m, err := system.LXDInstanceLiveMetrics(name)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, m)
}

func buildMapTopology(appCfg *config.AppConfig) mapTopology {
	top := mapTopology{TS: time.Now().Unix()}

	// ── Server header ──────────────────────────────────────────────────────
	top.Server = mapServer{
		Hostname:    hostnameOrEmpty(),
		VirtEnabled: system.LXDAvailable(),
	}
	if db := system.GetMetricsDB(); db != nil {
		top.Server.CPUPct = lastSample(db.Query("cpu_pct"))
		top.Server.MemPct = lastSample(db.Query("mem_used_pct"))
		var rx, tx float64
		for _, k := range db.Keys() {
			if strings.HasPrefix(k, "net_") && strings.HasSuffix(k, "_rx") {
				rx += lastSample(db.Query(k))
			} else if strings.HasPrefix(k, "net_") && strings.HasSuffix(k, "_tx") {
				tx += lastSample(db.Query(k))
			}
		}
		top.Server.NetRxMbps = rx
		top.Server.NetTxMbps = tx
	}

	// ── Live per-disk IO snapshot (from the 3s background poller) ──────────
	ioByDev := map[string]system.DiskIOSample{}
	if snap := system.GetDiskIOSnapshot(); snap != nil {
		ioByDev = snap.Devices
	}
	meta := diskMeta() // cached static model/serial/size, keyed by kernel name

	// ── ARC (system-wide; same value mirrored onto every pool) ─────────────
	var arcPct float64
	if arc, err := system.GetARCStats(); err == nil && arc != nil && arc.TotalRAMBytes > 0 {
		arcPct = float64(arc.ARCSize) / float64(arc.TotalRAMBytes) * 100
	}

	// ── Pools + their physical disks ───────────────────────────────────────
	// devToPool lets us map a dataset/zvol's pool name to a pool id, and a disk
	// to its owning pool.
	pools, _ := system.GetAllPools()
	for _, p := range pools {
		if p == nil {
			continue
		}
		poolID := "pool:" + p.Name
		mp := mapPool{
			ID:     poolID,
			Name:   p.Name,
			State:  p.Health,
			Used:   p.UsableUsed,
			Usable: p.UsableSize,
			ARCPct: arcPct,
		}

		// Members are parallel slices. Prefer resolved device paths.
		devs := p.MemberDevices
		if len(devs) == 0 {
			devs = p.Members
		}
		for i, dev := range devs {
			kn := devKernelName(dev)
			io := lookupIO(ioByDev, kn)
			mp.ReadKBps += io.ReadKBps
			mp.WriteKBps += io.WriteKBps

			d := mapDisk{
				ID:        "disk:" + kn,
				PoolID:    poolID,
				Dev:       kn,
				Status:    "ONLINE",
				Present:   true,
				ReadKBps:  io.ReadKBps,
				WriteKBps: io.WriteKBps,
				BusyPct:   io.BusyPct,
			}
			if i < len(p.MemberStatuses) {
				d.Status = p.MemberStatuses[i]
			}
			if i < len(p.MemberPresent) {
				d.Present = p.MemberPresent[i]
			}
			if i < len(p.MemberRoles) {
				d.Role = p.MemberRoles[i]
			}
			if dm, ok := meta[diskBase(kn)]; ok {
				d.Model = strings.TrimSpace(dm.Vendor + " " + dm.Model)
				d.Serial = dm.Serial
				d.SizeStr = dm.Size
				d.DiskType = dm.DiskType
			}
			top.Disks = append(top.Disks, d)
		}
		top.Pools = append(top.Pools, mp)
	}

	// ── Datasets & ZVols ───────────────────────────────────────────────────
	// dsByMount maps a mountpoint → dataset id (for SMB/NFS path resolution);
	// dsByName maps a zfs name → dataset id (for zvol/iSCSI + storage pools).
	dsByMount := map[string]string{}
	dsByName := map[string]string{}

	if datasets, err := system.ListAllDatasets(); err == nil {
		for _, ds := range datasets {
			id := "ds:" + ds.Name
			poolName := ds.Name
			if i := strings.IndexByte(ds.Name, '/'); i >= 0 {
				poolName = ds.Name[:i]
			}
			top.Datasets = append(top.Datasets, mapDataset{
				ID:        id,
				PoolID:    "pool:" + poolName,
				Kind:      "filesystem",
				Name:      ds.Name,
				Used:      ds.Used,
				Avail:     ds.Avail,
				Encrypted: ds.Encrypted,
			})
			dsByName[ds.Name] = id
			if ds.Mountpoint != "" && ds.Mountpoint != "none" && ds.Mountpoint != "legacy" {
				dsByMount[ds.Mountpoint] = id
			}
		}
	}
	if zvols, err := system.ListAllZVols(); err == nil {
		for _, zv := range zvols {
			id := "ds:" + zv.Name
			top.Datasets = append(top.Datasets, mapDataset{
				ID:        id,
				PoolID:    "pool:" + zv.Pool,
				Kind:      "zvol",
				Name:      zv.Name,
				Used:      zv.Used,
				Avail:     0,
				Encrypted: zv.Encrypted,
			})
			dsByName[zv.Name] = id
		}
	}

	// ── Consumers: SMB / NFS / iSCSI / S3 shares ───────────────────────────
	cfgDir := config.Dir()

	// Remotes are de-duplicated per client: one box per client, with a separate
	// link to every share it uses. SMB+NFS clients are keyed by IP (same machine
	// across protocols collapses to one box); iSCSI initiators by their IQN.
	remoteAcc := map[string]*mapRemote{}
	addRemote := func(key, label, ip, consumerID string) {
		r := remoteAcc[key]
		if r == nil {
			r = &mapRemote{ID: "rem:" + key, Label: label, IP: ip, Kind: "client", Dir: "unknown"}
			remoteAcc[key] = r
		}
		for _, id := range r.ConsumerIDs {
			if id == consumerID {
				return
			}
		}
		r.ConsumerIDs = append(r.ConsumerIDs, consumerID)
	}
	// SMB/NFS clients are resolved *after* VMs are built: a client whose IP
	// matches a known VM/container guest is folded into that VM as a lateral
	// link (see below) instead of getting its own remote box. We collect the
	// raw (share, client) pairs here and dispatch them once VM IPs are known.
	var pendingClients []pendingShareClient

	smbSessions := system.GetSMBSessions()
	if smbShares, err := system.ListSMBShares(cfgDir); err == nil {
		for _, s := range smbShares {
			c := mapConsumer{
				ID:         "smb:" + s.Name,
				Type:       "smb",
				Name:       s.Name,
				DatasetIDs: dsList(resolveByPath(dsByMount, s.Path)),
				Clients:    len(smbSessions[s.Name]),
			}
			top.Consumers = append(top.Consumers, c)
			for _, cl := range smbSessions[s.Name] {
				pendingClients = append(pendingClients, pendingShareClient{c.ID, cl})
			}
		}
	}

	nfsShares, _ := system.ListNFSShares(cfgDir)
	nfsSessions := system.GetNFSSessions(nfsShares)
	for _, s := range nfsShares {
		c := mapConsumer{
			ID:         "nfs:" + s.ID,
			Type:       "nfs",
			Name:       s.Path,
			DatasetIDs: dsList(resolveByPath(dsByMount, s.Path)),
			Clients:    len(nfsSessions[s.Path]),
		}
		top.Consumers = append(top.Consumers, c)
		for _, cl := range nfsSessions[s.Path] {
			pendingClients = append(pendingClients, pendingShareClient{c.ID, cl})
		}
	}

	if appCfg.ISCSI.Enabled {
		iscsiSessions := system.GetISCSISessions()
		for _, s := range appCfg.ISCSI.Shares {
			name := s.ZVol
			if name == "" {
				name = s.IQN
			}
			c := mapConsumer{
				ID:         "iscsi:" + s.ID,
				Type:       "iscsi",
				Name:       name,
				DatasetIDs: dsList(dsByName[s.ZVol]),
				Clients:    len(iscsiSessions[s.IQN]),
			}
			top.Consumers = append(top.Consumers, c)
			for _, initiator := range iscsiSessions[s.IQN] {
				addRemote("iscsi:"+initiator, initiator, "", c.ID)
			}
		}
	}

	if appCfg.MinIO.Enabled {
		// All buckets live on the single backend dataset.
		s3DatasetID := dsByName[strings.TrimPrefix(appCfg.MinIO.DatasetPath, "/")]
		if s3DatasetID == "" {
			s3DatasetID = resolveByPath(dsByMount, appCfg.MinIO.DataDir)
		}
		for _, b := range appCfg.MinIO.Buckets {
			top.Consumers = append(top.Consumers, mapConsumer{
				ID:         "s3:" + b.Name,
				Type:       "s3",
				Name:       b.Name,
				DatasetIDs: dsList(s3DatasetID),
			})
		}
	}

	// ── Consumers: VMs / containers (only when virtualization is available) ─
	if top.Server.VirtEnabled {
		// Incus storage-pool name → its backing zfs source dataset (e.g.
		// "default" → "NVMEPool/LXD-znas5"). A host may have several pools on
		// different zpools, so we resolve per instance, per disk device.
		poolSource := map[string]string{}
		for _, sp := range mustStoragePools() {
			if sp.Driver == "zfs" && sp.Source != "" {
				poolSource[sp.Name] = sp.Source
			}
		}
		disks := instanceDisks() // instance name → disk devices (cached 60s)
		if insts, err := system.LXDListInstanceSummaries(); err == nil {
			for _, in := range insts {
				typ := "container"
				if in.Type == "virtual-machine" {
					typ = "vm"
				}
				// Resolve every virtual disk to the zvol/dataset that backs it,
				// so each VM links to its actual disk zvol(s) — not just the pool.
				var dsIDs []string
				rootSrc := ""
				for _, dk := range disks[in.Name] {
					// Bind mount: a disk device with no storage pool whose source is
					// an absolute HOST path (e.g. /tank/media -> /data inside the CT).
					// Resolve it to the dataset whose mountpoint contains that path,
					// exactly like an SMB/NFS share path. Skips /dev/* passthrough and
					// host dirs not on any dataset (resolveByPath returns "").
					if dk.Pool == "" && strings.HasPrefix(dk.Source, "/") {
						if id := resolveByPath(dsByMount, dk.Source); id != "" {
							dsIDs = appendUniqueStr(dsIDs, id)
						}
						continue
					}
					src := poolSource[dk.Pool]
					if src == "" {
						continue
					}
					var cand string
					if dk.Path == "/" { // root disk
						rootSrc = src
						if typ == "vm" {
							cand = src + "/virtual-machines/" + in.Name + ".block"
						} else {
							cand = src + "/containers/" + in.Name
						}
					} else if dk.Source != "" { // attached custom volume
						cand = findCustomVol(dsByName, src, dk.Source)
					}
					if id, ok := dsByName[cand]; ok && cand != "" {
						dsIDs = appendUniqueStr(dsIDs, id)
					}
				}
				// Fallback: nothing resolved → link to the pool's root dataset.
				if len(dsIDs) == 0 && rootSrc != "" {
					if id, ok := dsByName[rootSrc]; ok {
						dsIDs = []string{id}
					}
				}
				top.Consumers = append(top.Consumers, mapConsumer{
					ID:         typ + ":" + in.Name,
					Type:       typ,
					Name:       in.Name,
					DatasetIDs: dsIDs,
					State:      in.State,
					IP:         in.IPv4,
				})
			}
		}
	}

	// ── Resolve SMB/NFS clients to VM lateral links or remote boxes ────────────
	foldShareClients(top.Consumers, pendingClients, addRemote)

	// ── Hide datasets/zvols nobody uses ────────────────────────────────────
	// A dataset/zvol only appears on the map if some consumer (SMB/NFS/iSCSI/S3
	// share, VM, or container) points at it. Orphan datasets add noise.
	referenced := make(map[string]bool, len(top.Consumers))
	for _, c := range top.Consumers {
		for _, id := range c.DatasetIDs {
			referenced[id] = true
		}
	}
	kept := top.Datasets[:0]
	for _, d := range top.Datasets {
		if referenced[d.ID] {
			kept = append(kept, d)
		}
	}
	top.Datasets = kept

	// Flush de-duplicated client remotes (stable order by id).
	clientKeys := make([]string, 0, len(remoteAcc))
	for k := range remoteAcc {
		clientKeys = append(clientKeys, k)
	}
	sort.Strings(clientKeys)
	for _, k := range clientKeys {
		top.Remotes = append(top.Remotes, *remoteAcc[k])
	}

	// ── Remote systems: interlink peers (header pills, not per-share) ──────
	for _, peer := range buildInterlinkPeerList(appCfg, "") {
		label := peer.Hostname
		if label == "" {
			label = peer.URL
		}
		top.Remotes = append(top.Remotes, mapRemote{
			ID:    "rem:peer:" + peer.URL,
			Label: label,
			Kind:  "peer", // peers attach to the server frame, not a consumer
			Dir:   "both",
		})
	}

	return top
}

// ── helpers ──────────────────────────────────────────────────────────────────

// pendingShareClient is one SMB/NFS session awaiting resolution to either a VM
// lateral link or a top-level remote box (decided once VM guest IPs are known).
type pendingShareClient struct {
	consumerID string
	cl         system.ShareClient
}

// foldShareClients routes each share client to either a VM/container it maps to
// by guest IP — recorded on that consumer's ClientOf so the frontend draws a
// lateral up-then-down link to the share instead of a duplicate client box — or,
// failing a match, to a normal remote client box via addRemote.
func foldShareClients(consumers []mapConsumer, clients []pendingShareClient, addRemote func(key, label, ip, consumerID string)) {
	vmByIP := map[string]int{} // guest IPv4 → index into consumers
	for i := range consumers {
		c := &consumers[i]
		if (c.Type == "vm" || c.Type == "container") && c.IP != "" {
			vmByIP[c.IP] = i
		}
	}
	for _, pc := range clients {
		if idx, ok := vmByIP[pc.cl.IP]; ok {
			consumers[idx].ClientOf = appendUniqueStr(consumers[idx].ClientOf, pc.consumerID)
			continue
		}
		label := pc.cl.FQDN
		if label == "" {
			label = pc.cl.IP
		}
		addRemote("client:"+pc.cl.IP, label, pc.cl.IP, pc.consumerID)
	}
}

func dsList(id string) []string {
	if id == "" {
		return nil
	}
	return []string{id}
}

func appendUniqueStr(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

func mustStoragePools() []system.LXDStoragePool {
	sps, err := system.LXDListStoragePoolInfos()
	if err != nil {
		return nil
	}
	return sps
}

// findCustomVol locates the zvol/dataset backing an Incus custom volume named
// `vol` on zfs source `src`. Incus names it "<src>/custom/<project>_<vol>", so
// we match by the "<src>/custom/" prefix and a "<vol>" suffix without needing
// to know the project string.
func findCustomVol(dsByName map[string]string, src, vol string) string {
	prefix := src + "/custom/"
	for name := range dsByName {
		if strings.HasPrefix(name, prefix) &&
			(strings.HasSuffix(name, "_"+vol) || strings.HasSuffix(name, "/"+vol) || name == prefix+vol) {
			return name
		}
	}
	return ""
}

// resolveByPath returns the dataset id whose mountpoint is the longest prefix of
// the given filesystem path (so a share rooted in a subdir maps to its dataset).
func resolveByPath(dsByMount map[string]string, path string) string {
	path = strings.TrimRight(path, "/")
	if path == "" {
		return ""
	}
	bestLen, bestID := -1, ""
	for mp, id := range dsByMount {
		m := strings.TrimRight(mp, "/")
		if path == m || strings.HasPrefix(path, m+"/") {
			if len(m) > bestLen {
				bestLen, bestID = len(m), id
			}
		}
	}
	return bestID
}

// lastSample returns the most recent value of an RRD series, or 0 if empty.
func lastSample(samples []rrd.Sample) float64 {
	if len(samples) == 0 {
		return 0
	}
	return samples[len(samples)-1].V
}

// lookupIO resolves a kernel device name to its IO sample, falling back to the
// whole-disk base name (sda1 → sda) when the exact key is absent.
func lookupIO(io map[string]system.DiskIOSample, kn string) system.DiskIOSample {
	if s, ok := io[kn]; ok {
		return s
	}
	if b := diskBase(kn); b != kn {
		if s, ok := io[b]; ok {
			return s
		}
	}
	return system.DiskIOSample{}
}

// devKernelName turns a device path ("/dev/sda") into a kernel name ("sda").
func devKernelName(dev string) string {
	dev = strings.TrimPrefix(dev, "/dev/")
	return dev
}

var (
	reNVMePartH = regexp.MustCompile(`^(nvme\d+n\d+|mmcblk\d+)p\d+$`)
	reSATAPartH = regexp.MustCompile(`^([a-z]+)\d+$`)
)

// diskBase strips a partition suffix from a kernel device name.
func diskBase(name string) string {
	if m := reNVMePartH.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	if m := reSATAPartH.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return name
}

func hostnameOrEmpty() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// ── static disk metadata cache (model/serial/size) ─────────────────────────
var (
	diskMetaMu      sync.Mutex
	diskMetaCache   map[string]system.DiskInfo
	diskMetaExpires time.Time
)

func diskMeta() map[string]system.DiskInfo {
	diskMetaMu.Lock()
	defer diskMetaMu.Unlock()
	if diskMetaCache != nil && time.Now().Before(diskMetaExpires) {
		return diskMetaCache
	}
	m := map[string]system.DiskInfo{}
	if disks, err := system.ListDisks(config.Dir()); err == nil {
		for _, d := range disks {
			m[d.Name] = d
		}
	}
	diskMetaCache = m
	diskMetaExpires = time.Now().Add(60 * time.Second)
	return m
}

// ── instance → disk-devices cache (60s) ─────────────────────────────────────
// Instance disk layout changes rarely, so we avoid the per-poll incus query by
// caching the batched lookup.
var (
	instDisksMu  sync.Mutex
	instDisksC   map[string][]system.LXDInstanceDisk
	instDisksExp time.Time
)

func instanceDisks() map[string][]system.LXDInstanceDisk {
	instDisksMu.Lock()
	defer instDisksMu.Unlock()
	if instDisksC != nil && time.Now().Before(instDisksExp) {
		return instDisksC
	}
	instDisksC = system.LXDAllInstanceDisks()
	instDisksExp = time.Now().Add(60 * time.Second)
	return instDisksC
}
