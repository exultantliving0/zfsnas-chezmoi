package system

import (
	"log"
	"path/filepath"
	"strings"
	"time"

	"zfsnas/internal/capacityrrd"
)

var wearoutDB *capacityrrd.DB

// GetWearoutDB returns the shared wear-out RRD database (nil until the collector
// has opened it). Exposed so a future lifetime-estimation feature can read the
// daily series and project when each SSD reaches 100% at the observed rate.
func GetWearoutDB() *capacityrrd.DB { return wearoutDB }

// StartWearoutCollector opens (or creates) config/wearout.rrd.json and starts a
// DAILY poller that records each SSD/NVMe's SMART wear-out percentage
// (0 = brand new, 100 = end of the manufacturer's rated write endurance). HDDs
// and any disk that doesn't report the metric (WearoutPct == nil) are skipped.
//
// This metric is collected purely for long-term retention — it is intentionally
// NOT plotted in any UI graph. It exists so we can later estimate, per SSD, when
// it will reach 100% wear-out by fitting the long-term rate from this history.
// It reuses the capacity RRD store (a generic multi-resolution float DB); the
// daily Tier-2 series (5-year retention) is the canonical long-term record.
func StartWearoutCollector(configDir string) {
	dbPath := filepath.Join(configDir, "wearout.rrd.json")
	db, err := capacityrrd.Open(dbPath)
	if err != nil {
		log.Printf("wearout: failed to open RRD at %s: %v", dbPath, err)
		return
	}
	wearoutDB = db

	go func() {
		// Give the daily SMART refresh a chance to populate the cache on a fresh
		// boot so the very first datapoint isn't all-nil, then sample once a day.
		time.Sleep(5 * time.Minute)
		sampleWearout(db, configDir, time.Now())
		if err := db.Flush(); err != nil {
			log.Printf("wearout: flush error: %v", err)
		}
		tick := time.NewTicker(24 * time.Hour)
		defer tick.Stop()
		for now := range tick.C {
			sampleWearout(db, configDir, now)
			if err := db.Flush(); err != nil {
				log.Printf("wearout: flush error: %v", err)
			}
		}
	}()
}

// wearoutKey is a stable per-disk series key. The serial number survives reboots
// and device-name churn (/dev/sdX reordering), so it's preferred; we fall back
// to model+device and finally the kernel name.
func wearoutKey(d DiskInfo) string {
	id := strings.TrimSpace(d.Serial)
	if id == "" {
		id = strings.TrimSpace(d.Model) + "_" + strings.TrimSpace(d.Device)
	}
	if id == "" || id == "_" {
		id = strings.TrimSpace(d.Name)
	}
	return "disk:" + id + ":wearout"
}

func sampleWearout(db *capacityrrd.DB, configDir string, now time.Time) {
	disks, err := ListDisks(configDir)
	if err != nil {
		log.Printf("wearout: ListDisks: %v", err)
		return
	}
	for _, d := range disks {
		if d.WearoutPct == nil {
			continue // HDD or no wear-out metric — nothing to record
		}
		db.Record(wearoutKey(d), float64(*d.WearoutPct), now)
	}
}
