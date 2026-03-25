package interlink

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const tokenTTL = 30 * time.Second

// GenerateToken creates a 30-second one-time SSO token signed with sharedSecret.
// Format: "<nonce>|<username>|<unix_timestamp>.<hex(HMAC-SHA256)>"
func GenerateToken(sharedSecret, username string) string {
	nonce := make([]byte, 8)
	rand.Read(nonce) //nolint:errcheck — rand.Read never fails in practice
	nonceHex := hex.EncodeToString(nonce)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	payload := nonceHex + "|" + username + "|" + ts
	mac := sign(sharedSecret, payload)
	return payload + "." + mac
}

// ValidateToken verifies the HMAC and checks expiry. Returns the embedded username.
func ValidateToken(sharedSecret, token string) (string, error) {
	dot := strings.LastIndex(token, ".")
	if dot < 0 {
		return "", errors.New("malformed token")
	}
	payload := token[:dot]
	mac := token[dot+1:]

	expected := sign(sharedSecret, payload)
	if !hmac.Equal([]byte(mac), []byte(expected)) {
		return "", errors.New("invalid token signature")
	}

	parts := strings.SplitN(payload, "|", 3)
	if len(parts) != 3 {
		return "", errors.New("malformed token payload")
	}
	username := parts[1]
	ts, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return "", fmt.Errorf("malformed timestamp: %w", err)
	}
	issued := time.Unix(ts, 0)
	if time.Since(issued) > tokenTTL {
		return "", errors.New("token expired")
	}
	return username, nil
}

func sign(sharedSecret, payload string) string {
	key, _ := hex.DecodeString(sharedSecret)
	if len(key) == 0 {
		key = []byte(sharedSecret)
	}
	h := hmac.New(sha256.New, key)
	h.Write([]byte(payload))
	return hex.EncodeToString(h.Sum(nil))
}
