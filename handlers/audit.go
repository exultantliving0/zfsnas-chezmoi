package handlers

import (
	"crypto/hmac"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"
	"zfsnas/internal/audit"
	"zfsnas/internal/config"
	"zfsnas/system"
)

// HandleAuditLog returns all audit log entries (local server only).
func HandleAuditLog(w http.ResponseWriter, r *http.Request) {
	entries, err := audit.Read()
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, "failed to read audit log")
		return
	}
	jsonOK(w, entries)
}

// HandleAuditAggregate returns the local audit log merged with every
// InterLink peer's. Each entry's System field identifies its origin
// (peer hostname for remote entries, local hostname for local ones).
// Entries are returned sorted descending by timestamp so the most recent
// activity across the whole InterLink fleet appears first.
//
// Per-peer fetch failures are silently skipped — the user still gets the
// rest of the data rather than an empty page when one peer is offline.
func HandleAuditAggregate(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		all, err := audit.Read()
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, "failed to read audit log")
			return
		}

		// Fan out to peers in parallel. 5-second per-peer timeout (enforced
		// by the interlinkClientFor http.Client we already use elsewhere).
		var (
			mu sync.Mutex
			wg sync.WaitGroup
		)
		for i := range appCfg.InterLink {
			ls := appCfg.InterLink[i]
			if ls.URL == "" {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				entries, err := system.GetRemoteAudit(ls.URL, ls.SharedSecret, ls.TLSFingerprint)
				if err != nil {
					return
				}
				// Belt-and-suspenders — peer's audit.Read already stamps
				// System, but if a peer were running an older version we
				// still want a hostname rather than blank.
				for j := range entries {
					if entries[j].System == "" {
						entries[j].System = ls.Hostname
					}
				}
				mu.Lock()
				all = append(all, entries...)
				mu.Unlock()
			}()
		}
		wg.Wait()

		// Newest first.
		sort.SliceStable(all, func(i, j int) bool {
			return all[i].Timestamp.After(all[j].Timestamp)
		})

		jsonOK(w, all)
	}
}

// HandleAuditPeerList serves a peer's request for the local audit log over
// the InterLink HMAC channel. Mirrors the validation pattern used by
// HandleLXDRemoteStoragePools and friends.
func HandleAuditPeerList(appCfg *config.AppConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req system.InterlinkAuditRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			jsonErr(w, http.StatusBadRequest, "invalid request body")
			return
		}
		age := time.Since(time.Unix(req.Timestamp, 0))
		if age > 30*time.Second || age < -5*time.Second {
			jsonErr(w, http.StatusUnauthorized, "request timestamp out of range")
			return
		}
		var matched bool
		for _, ls := range appCfg.InterLink {
			expected := system.InterlinkAuditHMAC(ls.SharedSecret, req.Timestamp, req.Nonce)
			if hmac.Equal([]byte(expected), []byte(req.HMAC)) {
				matched = true
				break
			}
		}
		if !matched {
			jsonErr(w, http.StatusUnauthorized, "invalid HMAC")
			return
		}
		entries, err := audit.Read()
		if err != nil {
			jsonErr(w, http.StatusInternalServerError, "audit read failed")
			return
		}
		jsonOK(w, map[string][]audit.Entry{"entries": entries})
	}
}
