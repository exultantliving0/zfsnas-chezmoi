package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"path/filepath"
	"syscall"
	"time"
	"zfsnas/handlers"
	"zfsnas/internal/alerts"
	"zfsnas/internal/audit"
	"zfsnas/internal/certgen"
	"zfsnas/internal/config"
	"zfsnas/internal/keystore"
	"zfsnas/internal/scheduler"
	"zfsnas/internal/session"
	"zfsnas/internal/version"
	"zfsnas/system"
)

//go:embed static
var embeddedStatic embed.FS

func main() {
	// ===== Flags =====
	devMode        := flag.Bool("dev", false, "Serve static files from disk (development mode)")
	debugMode      := flag.Bool("debug", false, "Enable verbose debug logging (lsblk details, etc.)")
	configDir      := flag.String("config", "./config", "Path to config directory")
	setHTTPSPort   := flag.Int("set-https-port", 0, "Persist a new HTTPS port to config and use it this run (1–65535)")
	experimentalMode := flag.Bool("experimental", false, "Enable experimental features (e.g. LXD VM/container management)")
	flag.Parse()

	// ===== Sudo check =====
	sudoStatus := system.CheckSudoAccess()
	if sudoStatus.Type == "none" {
		fmt.Fprintln(os.Stderr, "ERROR: zfsnas requires sudo access.")
		fmt.Fprintln(os.Stderr, "       See SECURITY.md for the recommended hardened sudoers configuration,")
		fmt.Fprintln(os.Stderr, "       or grant full passwordless access with:")
		fmt.Fprintln(os.Stderr, "         <your-user> ALL=(ALL) NOPASSWD: ALL")
		os.Exit(1)
	}
	if sudoStatus.Type == "hardened" && len(sudoStatus.MissingCommands) > 0 {
		fmt.Fprintf(os.Stderr, "WARNING: hardened sudo is configured but %d command(s) are missing from the sudoers rules: %s\n",
			len(sudoStatus.MissingCommands), strings.Join(sudoStatus.MissingCommands, ", "))
		fmt.Fprintln(os.Stderr, "         Some features may not work. See SECURITY.md for the full sudoers template.")
	}

	system.DebugMode = *debugMode
	if *debugMode {
		log.Println("Debug mode enabled — verbose logging active")
	}

	// ===== Config directory =====
	absConfig, err := filepath.Abs(*configDir)
	if err != nil {
		log.Fatalf("invalid config path: %v", err)
	}
	if err := config.Init(absConfig); err != nil {
		log.Fatalf("failed to init config dir %s: %v", absConfig, err)
	}
	log.Printf("Config directory: %s", absConfig)

	// ===== Encryption keystore =====
	if err := keystore.Init(absConfig); err != nil {
		log.Fatalf("failed to init keystore: %v", err)
	}

	// ===== Audit log =====
	audit.Init(absConfig)

	// ===== Alerts =====
	alerts.Init(absConfig)
	alertsHub := alerts.NewAlertsHub()
	alerts.SetWSHub(alertsHub)

	// ===== Scheduler =====
	scheduler.Init(absConfig)

	// ===== App config =====
	appCfg, err := config.LoadAppConfig()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}
	appCfg.ConfigDir = absConfig

	// ===== Persistent session store =====
	// Rehydrate any sessions persisted from a previous run so users
	// stay logged in across `systemctl restart zfsnas`. Failures are
	// non-fatal — if the file is missing or corrupted the session map
	// just starts empty and the user logs in once.
	if loaded, err := session.Default.BindPersistence(absConfig); err != nil {
		log.Printf("[sessions] persistence init failed: %v — sessions will not survive restart", err)
	} else if loaded > 0 {
		log.Printf("[sessions] restored %d session(s) from disk", loaded)
	}

	// ===== --set-https-port override =====
	if *setHTTPSPort != 0 {
		if *setHTTPSPort < 1 || *setHTTPSPort > 65535 {
			fmt.Fprintf(os.Stderr, "ERROR: --set-https-port must be between 1 and 65535 (got %d)\n", *setHTTPSPort)
			os.Exit(1)
		}
		appCfg.Port = *setHTTPSPort
		if err := config.SaveAppConfig(appCfg); err != nil {
			log.Fatalf("failed to save config with new port: %v", err)
		}
		log.Printf("HTTPS port updated to %d and saved to config.", *setHTTPSPort)
	}

	// ===== TLS certificates =====
	certsDir := filepath.Join(absConfig, "certs")
	if err := os.MkdirAll(certsDir, 0750); err != nil {
		log.Fatalf("failed to create certs directory: %v", err)
	}

	// Migrate legacy server.crt/server.key → self-signed.crt/self-signed.key
	legacyCert := filepath.Join(certsDir, "server.crt")
	legacyKey  := filepath.Join(certsDir, "server.key")
	selfCert   := filepath.Join(certsDir, "self-signed.crt")
	selfKey    := filepath.Join(certsDir, "self-signed.key")
	if certgen.Exists(legacyCert, legacyKey) && !certgen.Exists(selfCert, selfKey) {
		log.Println("Migrating server.crt/server.key → self-signed.crt/self-signed.key")
		os.Rename(legacyCert, selfCert)
		os.Rename(legacyKey, selfKey)
	}

	if !certgen.Exists(selfCert, selfKey) {
		log.Println("Generating self-signed TLS certificate…")
		if err := certgen.Generate(selfCert, selfKey); err != nil {
			log.Fatalf("failed to generate TLS cert: %v", err)
		}
		log.Printf("TLS certificate written to %s", certsDir)
	}

	// Determine active cert
	activeName := appCfg.ActiveCertName
	if activeName == "" {
		activeName = "self-signed"
	}
	certFile := filepath.Join(certsDir, activeName+".crt")
	keyFile  := filepath.Join(certsDir, activeName+".key")
	if !certgen.Exists(certFile, keyFile) {
		log.Printf("WARNING: active cert %q not found, falling back to self-signed", activeName)
		certFile = selfCert
		keyFile  = selfKey
	}

	// ===== Disk I/O poller (5-second samples for live charts) =====
	system.StartDiskIOPoller()

	// ===== Per-process CPU poller (3-second samples for top-bar gauge) =====
	system.StartCpuProcsPoller()

	// ===== Per-process memory poller (5-second samples for top-bar MEM gauge) =====
	system.StartMemProcsPoller()

	// ===== Metrics collector (5-minute samples for 24h RRD charts) =====
	system.StartMetricsCollector(absConfig)

	// ===== Capacity collector (5-minute samples, 3-tier RRD up to 5 years) =====
	system.StartCapacityCollector(absConfig)

	// ===== Global performance collector (CPU/mem/net/disk, 3-tier RRD up to 5 years) =====
	system.StartGlobalPerfCollector(absConfig)

	// ===== Pool performance collector (per-pool disk I/O, 3-tier RRD up to 5 years) =====
	system.StartPoolPerfCollector(absConfig)

	// ===== LXD VM/Container metrics collector (v6.4.28; gated by LXDMetricsEnabled) =====
	// Always start the goroutine; on each tick it consults getEnabled() to
	// decide whether to actually scrape. This lets the user flip the
	// Virtualization toggle at runtime without restarting the service.
	system.StartLXDMetricsCollector(absConfig, func() bool {
		c, _ := config.LoadAppConfig()
		return c != nil && c.LXDMetricsEnabled && system.LXDAvailable()
	})
	// Initial orphan sweep — catches RRD files for instances that were
	// deleted while the portal was offline.
	go func() {
		time.Sleep(10 * time.Second)
		if system.LXDAvailable() {
			system.SweepOrphanLXDMetrics()
		}
	}()

	// ===== LXD VM/Container state watcher (v6.5.3) =====
	// Polls instance statuses every 10s and writes an audit-log entry whenever
	// a VM or container changes state, including out-of-band changes (CLI
	// shutdown, qemu crash, host reboot recovery, autostart on boot). Costs
	// nothing on hosts that haven't enabled the feature — the loop short-
	// circuits when LXDAvailable() is false.
	//
	// Wire the alerts.Send dispatch for "VM stopped unexpectedly" — the
	// callback lives on the system package so we don't create an import
	// cycle (alerts already imports system via the interlink relay sub).
	system.OnVMUnexpectedStop = func(name, details, cause string) {
		subject := "[ZFS NAS] " + name + " stopped unexpectedly"
		body := "Instance: " + name + "\nState change: " + details + "\nDetected cause: " + cause + "\n"
		if err := alerts.Send(alerts.EventVMUnexpectedStop, subject, "vm_unexpected_stop", body); err != nil {
			log.Printf("[alerts] vm_unexpected_stop send failed for %s: %v", name, err)
		}
	}
	system.StartLXDStateWatcher()

	// ===== Daily SMART refresh goroutine =====
	handlers.StartDailySmartRefresh()

	// ===== Health alert poller =====
	handlers.StartHealthPoller(absConfig)

	// ===== Auto-load encryption keys for encrypted pools =====
	autoLoadEncryptionKeys(absConfig)

	// ===== Snapshot scheduler =====
	handlers.StartScheduler(appCfg)

	// ===== LXD snapshot + backup schedulers (v6.5.19+) =====
	handlers.StartLXDSnapshotScheduler(appCfg)
	handlers.StartLXDBackupScheduler(appCfg)

	// ===== Scrub scheduler =====
	handlers.StartScrubScheduler(appCfg)

	// ===== TreeMap scheduler =====
	handlers.StartTreeMapScheduler(appCfg)

	// ===== Auto-update scheduler =====
	handlers.StartAutoUpdateScheduler(appCfg)

	// ===== Recycle bin nightly cleaner =====
	system.StartRecycleCleaner(absConfig)

	// ===== One-time smb.conf deduplication (cleans up duplicate managed sections
	//       that may have been written by older versions of the software) =====
	if err := system.DeduplicateSMBConf(); err != nil {
		log.Printf("WARNING: smb.conf deduplication: %v", err)
	}

	// ===== Reapply smb.conf and /etc/exports from JSON on startup =====
	// This keeps the config files in sync with the JSON store even if a
	// previous write was interrupted or the binary was updated.
	if shares, err := system.ListSMBShares(absConfig); err == nil {
		if err := system.SaveSMBShares(absConfig, shares); err != nil {
			log.Printf("WARNING: startup smb.conf reapply: %v", err)
		}
	}
	if nfsShares, err := system.ListNFSShares(absConfig); err == nil {
		if err := system.SaveNFSShares(absConfig, nfsShares); err != nil {
			log.Printf("WARNING: startup /etc/exports reapply: %v", err)
		}
	}

	// ===== UPS RRD collector (5-min battery/runtime/load samples) =====
	system.StartUPSRRDCollector(absConfig, appCfg)

	// ===== UPS shutdown watcher =====
	go system.StartUPSShutdownWatcher(appCfg)

	// ===== Session cleanup goroutine =====
	go func() {
		t := time.NewTicker(30 * time.Minute)
		defer t.Stop()
		for range t.C {
			session.Default.CleanExpired()
		}
	}()

	// ===== Experimental features =====
	if *experimentalMode {
		version.SetExperimental(true)
		log.Println("Experimental mode enabled.")
		if system.LXDAvailable() {
			log.Println("Incus detected and accessible — VMs & Containers feature enabled.")
			handlers.SetLXDAvailable(true)
			// Ensure cross-distro OVMF firmware symlinks exist (Ubuntu ↔ Debian naming).
			system.EnsureOVMFCompat()
			// Background: sync Incus trust with all InterLink peers that don't
			// have it confirmed yet, so the flag is set without requiring manual
			// action. (Field name kept as LXDTrusted for config-file backwards
			// compat with 6.5.1 and earlier installs.)
			go func() {
				for i := range appCfg.InterLink {
					ls := &appCfg.InterLink[i]
					if ls.LXDTrusted {
						continue
					}
					if err := system.LXDSyncInterlinkTrustForPeer(*ls, ls.ID); err != nil {
						log.Printf("Incus trust auto-sync for %s: %v", ls.Hostname, err)
						continue
					}
					ls.LXDTrusted = true
					config.SaveAppConfig(appCfg) //nolint:errcheck
					log.Printf("Incus trust auto-synced for %s", ls.Hostname)
				}
			}()
		} else {
			log.Println("WARNING: Incus not accessible. Ensure the ZNAS user is in the '" + system.HVUserGroup + "' group. VMs & Containers feature disabled.")
		}
	}

	// ===== Static file system =====
	var staticFS fs.FS
	var readFile func(string) ([]byte, error)

	if *devMode {
		log.Println("Dev mode: serving static files from disk")
		staticFS = os.DirFS("static")
		readFile = func(name string) ([]byte, error) {
			return os.ReadFile(filepath.Join("static", name))
		}
	} else {
		sub, err := fs.Sub(embeddedStatic, "static")
		if err != nil {
			log.Fatalf("failed to create static sub-fs: %v", err)
		}
		staticFS = sub
		readFile = func(name string) ([]byte, error) {
			return embeddedStatic.ReadFile("static/" + name)
		}
	}

	// ===== Router =====
	router := handlers.NewRouter(staticFS, readFile, appCfg)

	// ===== First-run check =====
	users, err := config.LoadUsers()
	if err != nil {
		log.Fatalf("failed to load users: %v", err)
	}
	ip := localIP()
	if len(users) == 0 {
		log.Println("No users found — first-run setup required.")
		log.Printf("Open https://%s:%d/setup in your browser.", ip, appCfg.Port)
	} else {
		log.Printf("Loaded %d user(s).", len(users))
		log.Printf("Open https://%s:%d in your browser.", ip, appCfg.Port)
	}

	// ===== Interlink relay-mode notification subscribers =====
	// When global relay mode is enabled, dial /ws/alerts on every linked
	// server so admins on this box see in-app toasts from across the fleet.
	alerts.ReconcileLinkedServerSubscribers(appCfg)

	// ===== HTTP Server =====
	addr := fmt.Sprintf(":%d", appCfg.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      router,
		ReadHeaderTimeout: 15 * time.Second,
		WriteTimeout:      300 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Optional second listener on the standard HTTPS port 443 — Settings →
	// Server Port → "Also bind port 443". Binding a privileged port from the
	// non-root service account requires CAP_NET_BIND_SERVICE (granted to the
	// systemd unit via AmbientCapabilities). If that capability is missing the
	// bind fails; we log a clear warning and leave the primary listener up.
	var srv443 *http.Server
	if appCfg.BindPort443 && appCfg.Port != 443 {
		srv443 = &http.Server{
			Addr:              ":443",
			Handler:           router,
			ReadHeaderTimeout: 15 * time.Second,
			WriteTimeout:      300 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		go func() {
			log.Printf("HTTPS server also listening on :443")
			if err := srv443.ListenAndServeTLS(certFile, keyFile); err != nil && err != http.ErrServerClosed {
				log.Printf("WARNING: could not bind port 443 (%v) — the zfsnas.service unit may be missing AmbientCapabilities=CAP_NET_BIND_SERVICE; the portal stays reachable on :%d", err, appCfg.Port)
			}
		}()
	}

	// Graceful shutdown on SIGINT / SIGTERM.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Printf("HTTPS server listening on %s", addr)
		if err := srv.ListenAndServeTLS(certFile, keyFile); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	<-stop
	log.Println("Shutting down…")
	// Flush the latest session activity heartbeats to disk before exit so
	// a clean restart doesn't lose the LastActivityAt timestamps the
	// inactivity-timeout enforcement depends on.
	session.Default.FlushNow()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	if srv443 != nil {
		srv443.Shutdown(ctx)
	}
	log.Println("Server stopped.")
}

// autoLoadEncryptionKeys scans all pools and datasets, loads any managed
// encryption key whose keystatus is "unavailable", then mounts the dataset.
// It also mounts any encrypted dataset whose key is already available but not yet mounted.
func autoLoadEncryptionKeys(configDir string) {
	// --- Pass 1: load managed keys for locked datasets ---
	keys, _ := config.LoadEncryptionKeys()
	if len(keys) > 0 {
		keyMap := make(map[string]string, len(keys))
		for _, k := range keys {
			keyMap[k.ID] = keystore.KeyFilePath(k.ID)
		}

		type target struct{ name string }
		var targets []target
		pools, _ := system.GetAllPools()
		for _, p := range pools {
			if p.Encrypted && p.KeyLocked {
				targets = append(targets, target{p.Name})
			}
		}
		datasets, _ := system.ListAllDatasets()
		for _, d := range datasets {
			if d.Encrypted && d.KeyLocked {
				targets = append(targets, target{d.Name})
			}
		}
		for _, t := range targets {
			loc := system.GetKeyLocation(t.name)
			if !strings.HasPrefix(loc, "file://") {
				continue
			}
			base := filepath.Base(strings.TrimPrefix(loc, "file://"))
			id := strings.TrimSuffix(base, ".key")
			keyPath, ok := keyMap[id]
			if !ok || !keystore.Exists(id) {
				log.Printf("encryption: key %s for %s not found — will remain locked", id, t.name)
				continue
			}
			if err := system.LoadPoolKey(t.name, keyPath); err != nil {
				log.Printf("encryption: failed to load key for %s: %v", t.name, err)
				continue
			}
			log.Printf("encryption: loaded key for %s", t.name)
			if err := system.MountDataset(t.name); err != nil {
				log.Printf("encryption: mount %s: %v", t.name, err)
			}
		}
	}

	// --- Pass 2: mount any encrypted dataset whose key is available but not yet mounted ---
	// Runs regardless of managed keys — handles pools imported with their own keylocation.
	datasets, _ := system.ListAllDatasets()
	for _, d := range datasets {
		if d.Encrypted && !d.KeyLocked && !d.Mounted &&
			d.Mountpoint != "none" && d.Mountpoint != "legacy" && d.CanMount != "off" {
			log.Printf("encryption: mounting unlocked-but-unmounted dataset %s", d.Name)
			if err := system.MountDataset(d.Name); err != nil {
				log.Printf("encryption: mount %s: %v", d.Name, err)
			}
		}
	}
}

// localIP returns the primary non-loopback IPv4 address of the host.
// Falls back to "localhost" if none can be determined.
func localIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "localhost"
	}
	defer conn.Close()
	addr := conn.LocalAddr().(*net.UDPAddr)
	return addr.IP.String()
}
