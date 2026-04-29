package system

// HMAC-authenticated audit-log fan-out so the local Activity & Events page
// can show entries from every InterLink peer side-by-side. Same shape as
// the other interlink endpoints (lxd-storage, vm-nvram-reset, etc.) — a
// timestamped + nonced + HMAC'd POST body validated within ±30s of now.

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
	"zfsnas/internal/audit"
)

// InterlinkAuditHMAC signs a request to fetch a peer's audit log.
func InterlinkAuditHMAC(sharedSecret string, timestamp int64, nonce string) string {
	key, _ := hex.DecodeString(sharedSecret)
	if len(key) == 0 {
		key = []byte(sharedSecret)
	}
	h := hmac.New(sha256.New, key)
	h.Write([]byte("audit-list|" + strconv.FormatInt(timestamp, 10) + "|" + nonce))
	return hex.EncodeToString(h.Sum(nil))
}

// InterlinkAuditRequest is the HMAC-signed POST body sent to a peer.
type InterlinkAuditRequest struct {
	Timestamp int64  `json:"timestamp"`
	Nonce     string `json:"nonce"`
	HMAC      string `json:"hmac"`
}

// GetRemoteAudit asks a peer ZNAS server for its audit log entries.
// Each returned entry already has its System field stamped by the peer's
// audit.Read, so callers don't need to add the hostname themselves.
func GetRemoteAudit(remoteURL, sharedSecret, tlsFP string) ([]audit.Entry, error) {
	ts := time.Now().Unix()
	nonceBytes := make([]byte, 8)
	rand.Read(nonceBytes) //nolint:errcheck
	nh := hex.EncodeToString(nonceBytes)
	req := InterlinkAuditRequest{
		Timestamp: ts,
		Nonce:     nh,
		HMAC:      InterlinkAuditHMAC(sharedSecret, ts, nh),
	}
	body, _ := json.Marshal(req)
	resp, err := interlinkClientFor(tlsFP).Post(
		remoteURL+"/api/audit/peer-list", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("audit/peer-list returned status %d", resp.StatusCode)
	}
	var r struct {
		Entries []audit.Entry `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	return r.Entries, nil
}
