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
	"zfsnas/system"
)

//go:embed static
var embeddedStatic embed.FS

func main() {
	// ===== Flags =====
	devMode      := flag.Bool("dev", false, "Serve static files from disk (development mode)")
	debugMode    := flag.Bool("debug", false, "Enable verbose debug logging (lsblk details, etc.)")
	configDir    := flag.String("config", "./config", "Path to config directory")
	setHTTPSPort := flag.Int("set-https-port", 0, "Persist a new HTTPS port to config and use it this run (1–65535)")
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

	// ===== Metrics collector (5-minute samples for 24h RRD charts) =====
	system.StartMetricsCollector(absConfig)

	// ===== Capacity collector (5-minute samples, 3-tier RRD up to 5 years) =====
	system.StartCapacityCollector(absConfig)

	// ===== Global performance collector (CPU/mem/net/disk, 3-tier RRD up to 5 years) =====
	system.StartGlobalPerfCollector(absConfig)

	// ===== Pool performance collector (per-pool disk I/O, 3-tier RRD up to 5 years) =====
	system.StartPoolPerfCollector(absConfig)

	// ===== Daily SMART refresh goroutine =====
	handlers.StartDailySmartRefresh()

	// ===== Health alert poller =====
	handlers.StartHealthPoller(absConfig)

	// ===== Auto-load encryption keys for encrypted pools =====
	autoLoadEncryptionKeys(absConfig)

	// ===== Snapshot scheduler =====
	handlers.StartScheduler()

	// ===== Scrub scheduler =====
	handlers.StartScrubScheduler(appCfg)

	// ===== TreeMap scheduler =====
	handlers.StartTreeMapScheduler(appCfg)

	// ===== Recycle bin nightly cleaner =====
	system.StartRecycleCleaner(absConfig)

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

	// ===== HTTP Server =====
	addr := fmt.Sprintf(":%d", appCfg.Port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 300 * time.Second,
		IdleTimeout:  120 * time.Second,
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
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
