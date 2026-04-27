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
// Series keys:
//   read:{pool}:{dev}, write:{pool}:{dev}, busy:{pool}:{dev}
//   l2size:{pool}      — bytes currently held in L2ARC (system-wide arcstats)
//   l2hitpct:{pool}    — L2 hit % over the 5-minute window
// L2 series are emitted only for pools that have at least one cache device.
// Because arcstats expose system-wide L2 counters (no per-pool breakdown), the
// same numeric values are written for every pool that has L2ARC.
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

		// Prime L2 hit/miss counters so the first tick can compute a delta.
		var prevL2Hits, prevL2Misses int64
		if arc, err := GetARCStats(); err == nil {
			prevL2Hits, prevL2Misses = arc.L2Hits, arc.L2Misses
		}

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

			// L2ARC sampling — only when at least one pool has cache devices.
			// arcstats are system-wide, so the same values are recorded for every
			// pool that currently exposes L2ARC.
			if pools, perr := GetAllPools(); perr == nil {
				var l2Pools []string
				for _, p := range pools {
					if p != nil && len(p.CacheDevs) > 0 {
						l2Pools = append(l2Pools, p.Name)
					}
				}
				if len(l2Pools) > 0 {
					if arc, aerr := GetARCStats(); aerr == nil {
						dHits := arc.L2Hits - prevL2Hits
						dMiss := arc.L2Misses - prevL2Misses
						var hitPct float64
						if dHits < 0 || dMiss < 0 {
							// Counter reset (module reload) — skip this sample.
							hitPct = -1
						} else if total := dHits + dMiss; total > 0 {
							hitPct = float64(dHits) / float64(total) * 100
						}
						for _, pool := range l2Pools {
							db.Record("l2size:"+pool, float64(arc.L2Size), now)
							if hitPct >= 0 {
								db.Record("l2hitpct:"+pool, hitPct, now)
							}
						}
						prevL2Hits, prevL2Misses = arc.L2Hits, arc.L2Misses
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
