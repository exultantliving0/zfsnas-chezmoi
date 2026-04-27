package handlers

import (
	"net"
	"net/http"
	"os"
	"os/user"
	"zfsnas/internal/version"
)

// HandleGetVersion returns the running application version, releases URL, server IP, and hostname.
func HandleGetVersion(w http.ResponseWriter, r *http.Request) {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "localhost"
	}
	username := "zfsnas"
	if u, err := user.Current(); err == nil && u.Username != "" {
		username = u.Username
	}
	jsonOK(w, map[string]interface{}{
		"version":          version.Version,
		"releases_url":     version.ReleasesURL,
		"server_ip":        serverIP(),
		"hostname":         hostname,
		"username":         username,
		"experimental_mode": version.IsExperimental(),
	})
}

// serverIP returns the primary non-loopback IPv4 address.
func serverIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "localhost"
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String()
}
