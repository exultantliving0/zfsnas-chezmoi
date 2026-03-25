package system

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// interlinkClient skips TLS verification — self-signed certs are normal on a LAN.
var interlinkClient = &http.Client{
	Timeout: 30 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
	},
}

// InterlinkPingResponse is what /api/interlink/ping returns.
type InterlinkPingResponse struct {
	Hostname string `json:"hostname"`
	Version  string `json:"version"`
	OK       bool   `json:"ok"`
}

// PingServer calls GET <url>/api/interlink/ping and returns the remote hostname + version.
func PingServer(url string) (hostname, version string, err error) {
	resp, err := interlinkClient.Get(url + "/api/interlink/ping")
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("ping returned status %d", resp.StatusCode)
	}
	var p InterlinkPingResponse
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		return "", "", err
	}
	return p.Hostname, p.Version, nil
}

// AcceptLinkRequest is the payload sent to the remote server during linking.
type AcceptLinkRequest struct {
	CallerURL      string `json:"caller_url"`
	CallerHostname string `json:"caller_hostname"`
	CallerID       string `json:"caller_id"`
	SharedSecret   string `json:"shared_secret"`
	AdminUsername  string `json:"admin_username"`
	AdminPassword  string `json:"admin_password"`
	AdminTOTP      string `json:"admin_totp,omitempty"`
}

// AcceptLinkResponse is what the remote server returns.
type AcceptLinkResponse struct {
	RemoteID   string `json:"remote_id"`
	Hostname   string `json:"hostname"`
	TOTPNeeded bool   `json:"totp_needed"`
}

// SendAcceptLink calls POST <url>/api/interlink/accept-link.
func SendAcceptLink(url string, req AcceptLinkRequest) (*AcceptLinkResponse, error) {
	body, _ := json.Marshal(req)
	resp, err := interlinkClient.Post(url+"/api/interlink/accept-link", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		var e struct{ Error string `json:"error"` }
		json.NewDecoder(resp.Body).Decode(&e) //nolint:errcheck
		if e.Error != "" {
			return nil, fmt.Errorf("remote rejected credentials: %s", e.Error)
		}
		return nil, fmt.Errorf("remote rejected credentials")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("accept-link returned status %d", resp.StatusCode)
	}
	var r AcceptLinkResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

// CheckUserRequest is the HMAC-signed payload for /api/interlink/check-user.
type CheckUserRequest struct {
	Username  string `json:"username"`
	Timestamp int64  `json:"timestamp"`
	Nonce     string `json:"nonce"`
	HMAC      string `json:"hmac"`
}

// CheckUserResponse is what the remote server returns.
type CheckUserResponse struct {
	Exists bool `json:"exists"`
}

// CheckUserOnRemote calls POST <url>/api/interlink/check-user, signing with sharedSecret.
func CheckUserOnRemote(remoteURL, sharedSecret, username string) (*CheckUserResponse, error) {
	nonce := make([]byte, 8)
	rand.Read(nonce) //nolint:errcheck
	req := CheckUserRequest{
		Username:  username,
		Timestamp: time.Now().Unix(),
		Nonce:     hex.EncodeToString(nonce),
	}
	// HMAC payload: "<username>|<timestamp>|<nonce>"
	req.HMAC = checkUserHMAC(sharedSecret, req.Username, req.Timestamp, req.Nonce)

	body, _ := json.Marshal(req)
	resp, err := interlinkClient.Post(remoteURL+"/api/interlink/check-user", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("check-user returned status %d", resp.StatusCode)
	}
	var r CheckUserResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

// CheckUserHMAC computes the HMAC for a check-user request.
// Exported so handlers/interlink.go can verify inbound requests.
func CheckUserHMAC(sharedSecret, username string, timestamp int64, nonce string) string {
	return checkUserHMAC(sharedSecret, username, timestamp, nonce)
}

func checkUserHMAC(sharedSecret, username string, timestamp int64, nonce string) string {
	key, _ := hex.DecodeString(sharedSecret)
	if len(key) == 0 {
		key = []byte(sharedSecret)
	}
	h := hmac.New(sha256.New, key)
	h.Write([]byte(username + "|" + strconv.FormatInt(timestamp, 10) + "|" + nonce))
	return hex.EncodeToString(h.Sum(nil))
}

// GenerateSharedSecret creates a random 32-byte hex secret.
func GenerateSharedSecret() string {
	b := make([]byte, 32)
	rand.Read(b) //nolint:errcheck
	return hex.EncodeToString(b)
}

// RemoteUnlinkRequest is the HMAC-signed payload for /api/interlink/remote-unlink.
// RemoteID is our local ID as known by the receiving server.
type RemoteUnlinkRequest struct {
	RemoteID  string `json:"remote_id"`
	Timestamp int64  `json:"timestamp"`
	Nonce     string `json:"nonce"`
	HMAC      string `json:"hmac"`
}

// RemoteUnlinkHMAC computes the HMAC for a remote-unlink request.
func RemoteUnlinkHMAC(sharedSecret, remoteID string, timestamp int64, nonce string) string {
	key, _ := hex.DecodeString(sharedSecret)
	if len(key) == 0 {
		key = []byte(sharedSecret)
	}
	h := hmac.New(sha256.New, key)
	h.Write([]byte("unlink|" + remoteID + "|" + strconv.FormatInt(timestamp, 10) + "|" + nonce))
	return hex.EncodeToString(h.Sum(nil))
}

// SendRemoteUnlink calls POST <url>/api/interlink/remote-unlink, asking the remote
// server to remove us from its linked-server list. Errors are ignored (best-effort).
func SendRemoteUnlink(remoteURL, sharedSecret, remoteID string) error {
	nonce := make([]byte, 8)
	rand.Read(nonce) //nolint:errcheck
	ts := time.Now().Unix()
	req := RemoteUnlinkRequest{
		RemoteID:  remoteID,
		Timestamp: ts,
		Nonce:     hex.EncodeToString(nonce),
		HMAC:      RemoteUnlinkHMAC(sharedSecret, remoteID, ts, hex.EncodeToString(nonce)),
	}
	body, _ := json.Marshal(req)
	resp, err := interlinkClient.Post(remoteURL+"/api/interlink/remote-unlink", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("remote-unlink returned status %d", resp.StatusCode)
	}
	return nil
}
