package system

import (
	"log"
	"path/filepath"
	"time"
	"zfsnas/internal/capacityrrd"
)

var poolPerfDB *capacityrrd.DB

// GetPoolPerfDB returns the shared per-pool disk performance RRD database.
func GetPoolPerfDB() *capacityrrd.DB {
	return poolPerfDB
}

// StartPoolPerfCollector opens (or creates) the per-pool disk performance RRD
// and starts a goroutine that samples disk I/O per pool every 5 minutes.
// Series keys: read:{pool}:{dev}, write:{pool}:{dev}, busy:{pool}:{dev}
func StartPoolPerfCollector(configDir string) {
	dbPath := filepath.Join(configDir, "pool_perf.rrd.json")
	db, err := capacityrrd.Open(dbPath)
	if err != nil {
		log.Printf("pool_perf: failed to open RRD at %s: %v", dbPath, err)
		return
	}
	poolPerfDB = db

	go func() {
		// Prime prevDiskIO immediately so the first tick (5 min from now)
		// can compute a delta rather than requiring two full ticks.
		poolDevs := poolMemberBaseNamesPerPool()
		var allDevs []string
		if len(poolDevs) > 0 {
			seen := make(map[string]bool)
			for _, devs := range poolDevs {
				for _, d := range devs {
					if !seen[d] {
						seen[d] = true
						allDevs = append(allDevs, d)
					}
				}
			}
		}
		prevDiskIO, _ := readDiskstats(allDevs)
		prevDiskTime := time.Now()

		tick := time.NewTicker(5 * time.Minute)
		defer tick.Stop()

		for now := range tick.C {
			poolDevs = poolMemberBaseNamesPerPool()
			if len(poolDevs) == 0 {
				continue
			}

			// Collect union of all devices for a single diskstats read.
			seen := make(map[string]bool)
			allDevs = nil
			for _, devs := range poolDevs {
				for _, d := range devs {
					if !seen[d] {
						seen[d] = true
						allDevs = append(allDevs, d)
					}
				}
			}

			curDisk, err := readDiskstats(allDevs)
			if err == nil && prevDiskIO != nil {
				dtSec := now.Sub(prevDiskTime).Seconds()
				if dtSec > 0 {
					for pool, devs := range poolDevs {
						for _, dev := range devs {
							cur, hasCur := curDisk[dev]
							prev, hasPrev := prevDiskIO[dev]
							if !hasCur || !hasPrev {
								continue
							}
							readMBps  := float64(cur.sectorsRead-prev.sectorsRead)       * 512 / 1_048_576 / dtSec
							writeMBps := float64(cur.sectorsWritten-prev.sectorsWritten) * 512 / 1_048_576 / dtSec
							dtMS := dtSec * 1000
							busy := float64(cur.msIO-prev.msIO) / dtMS * 100
							if busy > 100 {
								busy = 100
							}
							db.Record("read:"+pool+":"+dev,  readMBps,  now)
							db.Record("write:"+pool+":"+dev, writeMBps, now)
							db.Record("busy:"+pool+":"+dev,  busy,      now)
						}
					}
				}
			}

			prevDiskIO  = curDisk
			prevDiskTime = now

			if err := db.Flush(); err != nil {
				log.Printf("pool_perf: flush error: %v", err)
			}
		}
	}()
}
