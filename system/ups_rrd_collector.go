package system

import (
	"log"
	"path/filepath"
	"time"
	"zfsnas/internal/capacityrrd"
	"zfsnas/internal/config"
)

var upsRRD *capacityrrd.DB

// GetUPSRRD returns the shared UPS RRD database (nil until started).
func GetUPSRRD() *capacityrrd.DB {
	return upsRRD
}

// StartUPSRRDCollector opens (or creates) the UPS RRD and starts the
// 5-minute background poller that samples battery charge, runtime, and load.
func StartUPSRRDCollector(configDir string, appCfg *config.AppConfig) {
	dbPath := filepath.Join(configDir, "ups.rrd.json")
	db, err := capacityrrd.Open(dbPath)
	if err != nil {
		log.Printf("ups-rrd: failed to open RRD at %s: %v", dbPath, err)
		return
	}
	upsRRD = db

	go func() {
		tick := time.NewTicker(5 * time.Minute)
		defer tick.Stop()
		for now := range tick.C {
			sampleUPSRRD(db, appCfg, now)
			if err := db.Flush(); err != nil {
				log.Printf("ups-rrd: flush error: %v", err)
			}
		}
	}()
}

func sampleUPSRRD(db *capacityrrd.DB, appCfg *config.AppConfig, now time.Time) {
	var status *UPSStatus

	// NUT not installed → sample the sysfs battery whenever one is present.
	// This mirrors HandleGetUPSStatus, which surfaces the UPS panel based on
	// battery detection alone (no explicit ups.enabled toggle exists for the
	// sysfs path — saveUPSConfig only writes shutdown policy in that mode).
	// Without this, the Performance UPS tab would stay empty on laptop/SBC
	// hosts even though the panel is visible.
	if !UPSPrereqsInstalled() {
		status = QuerySysBattery()
		if status == nil {
			return
		}
	} else {
		// NUT installed → require explicit ups.enabled (with a configured
		// device or remote target) before consuming `upsc` cycles every 5 min.
		ups := appCfg.UPS
		if !ups.Enabled {
			return
		}
		mode := ups.Mode
		if mode == "" {
			mode = "standalone"
		}
		var err error
		switch mode {
		case "network_client":
			if ups.NUTClient == nil || ups.NUTClient.Host == "" {
				return
			}
			status, err = QueryUPSClient(ups.NUTClient)
		default: // standalone or network_server
			if ups.UPSName == "" {
				return
			}
			status, err = QueryUPS(ups.UPSName)
		}
		if err != nil {
			return
		}
	}

	if status.ChargePct != nil {
		db.Record("ups:charge_pct", *status.ChargePct, now)
	}
	if status.RuntimeSecs != nil {
		db.Record("ups:runtime_secs", float64(*status.RuntimeSecs), now)
	}
	if status.LoadPct != nil {
		db.Record("ups:load_pct", *status.LoadPct, now)
	}
}
