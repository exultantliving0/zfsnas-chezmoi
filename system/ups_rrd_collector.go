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
	ups := appCfg.UPS
	if !ups.Enabled || ups.UPSName == "" {
		return
	}
	if !UPSPrereqsInstalled() {
		return
	}
	status, err := QueryUPS(ups.UPSName)
	if err != nil {
		return
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
