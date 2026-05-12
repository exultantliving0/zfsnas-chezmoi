package alerts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"zfsnas/internal/config"
	"zfsnas/system"

	"github.com/gorilla/websocket"
)

// interlink_relay_sub.go — when the global "Interlink Relay Mode" setting is
// enabled, this file maintains one outbound WebSocket subscriber per
// LinkedServer in appCfg.InterLink. Each subscriber dials the remote's
// /ws/alerts endpoint (authenticated via the standard X-Interlink-Relay-*
// HMAC headers — accepted by RelayAuthMiddleware on the remote) and forwards
// every received alert payload to all admin browser sessions on this local
// box via AlertsHub.BroadcastJSONToAdmins.
//
// Triggers that call Reconcile:
//   - daemon startup (main.go)
//   - relay-mode toggle (HandleInterlinkSetRelayMode)
//   - link add / remove / edit (HandleInterlinkAcceptLink, *Unlink, etc.)
//
// Reconcile diffs the desired set of (server ID → subscriber) against the
// running set, cancelling stale ones and starting missing ones.

type relaySubscriber struct {
	serverID string
	cancel   context.CancelFunc
}

var (
	relaySubMu sync.Mutex
	// keyed by LinkedServer.ID so we can cheaply diff against appCfg.InterLink
	relaySubs = map[string]*relaySubscriber{}
)

// ReconcileLinkedServerSubscribers brings the running subscriber set in line
// with appCfg: one subscriber per LinkedServer when InterlinkRelayMode is on,
// none when it's off. Safe to call repeatedly.
func ReconcileLinkedServerSubscribers(appCfg *config.AppConfig) {
	if wsHub == nil || appCfg == nil {
		return
	}

	desired := map[string]config.LinkedServer{}
	if appCfg.InterlinkRelayMode {
		for _, ls := range appCfg.InterLink {
			if ls.URL == "" || ls.SharedSecret == "" || ls.LinkedBy == "" {
				continue
			}
			desired[ls.ID] = ls
		}
	}

	relaySubMu.Lock()
	defer relaySubMu.Unlock()

	// Cancel subscribers that no longer belong.
	for id, sub := range relaySubs {
		if _, keep := desired[id]; !keep {
			sub.cancel()
			delete(relaySubs, id)
		}
	}

	// Start subscribers that don't exist yet.
	for id, ls := range desired {
		if _, running := relaySubs[id]; running {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		relaySubs[id] = &relaySubscriber{serverID: id, cancel: cancel}
		go runRelaySubscriber(ctx, ls.URL, ls.SharedSecret, ls.TLSFingerprint, ls.LinkedBy, ls.Hostname)
	}
}

func runRelaySubscriber(ctx context.Context, remoteURL, sharedSecret, tlsFingerprint, username, remoteHostname string) {
	// Convert https:// → wss:// (http → ws as a safety net for non-TLS test setups).
	wsURL := remoteURL
	switch {
	case strings.HasPrefix(wsURL, "https://"):
		wsURL = "wss://" + wsURL[len("https://"):]
	case strings.HasPrefix(wsURL, "http://"):
		wsURL = "ws://" + wsURL[len("http://"):]
	}
	wsURL = strings.TrimRight(wsURL, "/") + "/ws/alerts"

	// Exponential backoff: 1s → 2 → 4 → 8 → 16 → cap at 30. Reset on success.
	backoff := time.Second
	const maxBackoff = 30 * time.Second

	for {
		if ctx.Err() != nil {
			return
		}

		err := dialAndPumpRelay(ctx, wsURL, sharedSecret, tlsFingerprint, username, remoteHostname)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("[alerts/relay-sub] %s: %v (retry in %s)", remoteHostname, err, backoff)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

func dialAndPumpRelay(ctx context.Context, wsURL, sharedSecret, tlsFingerprint, username, remoteHostname string) error {
	// Build fresh HMAC for this dial (the remote rejects timestamps older than 30s).
	ts := time.Now().Unix()
	nonceBytes := make([]byte, 8)
	if _, err := rand.Read(nonceBytes); err != nil {
		return err
	}
	nonceHex := hex.EncodeToString(nonceBytes)
	sig := system.RelayForwardHMAC(sharedSecret, username, ts, nonceHex)

	header := http.Header{}
	header.Set("X-Interlink-Relay-User", username)
	header.Set("X-Interlink-Relay-TS", strconv.FormatInt(ts, 10))
	header.Set("X-Interlink-Relay-Nonce", nonceHex)
	header.Set("X-Interlink-Relay-HMAC", sig)

	dialer := websocket.Dialer{
		TLSClientConfig:  system.InterlinkTLSConfigForRelay(tlsFingerprint),
		HandshakeTimeout: 15 * time.Second,
	}

	conn, _, err := dialer.DialContext(ctx, wsURL, header)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Cancellation: close the conn from a goroutine watching the context so
	// the blocking ReadMessage unblocks.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	log.Printf("[alerts/relay-sub] %s: subscribed to remote alerts", remoteHostname)

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var payload map[string]any
		if err := json.Unmarshal(msg, &payload); err != nil {
			continue
		}
		// Loop guard: if the message is already a forwarded one (it carries
		// from_relay=true), drop it. Without this, A→B→A→B feedback loops
		// occur on every event. The per-connection role="interlink" filter
		// in HandleAlertsWS also prevents the loop, but this is a cheap
		// defense in depth and works even in multi-hop fleets.
		if v, ok := payload["from_relay"].(bool); ok && v {
			continue
		}
		// The remote already populates "hostname" with its own os.Hostname(),
		// so the toast naturally shows the source server. Tag with from_relay
		// so the UI can apply different styling later if desired.
		payload["from_relay"] = true
		wsHub.BroadcastJSONToAdmins(payload)
	}
}
