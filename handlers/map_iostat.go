package handlers

import (
	"bufio"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"

	"github.com/gorilla/websocket"

	"zfsnas/system"
)

var iostatUpgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

// HandleZpoolIostatWS streams one-second `zpool iostat` samples for a pool over
// a WebSocket, used by the Map hovercard to draw live bandwidth/ops charts.
// Each message is {"r_bw","w_bw","r_ops","w_ops"} (bytes/s and ops/s).
// GET /ws/zpool-iostat?pool=<name>
func HandleZpoolIostatWS(w http.ResponseWriter, r *http.Request) {
	pool := r.URL.Query().Get("pool")
	if pool == "" || len(pool) > 64 {
		http.Error(w, "invalid pool", http.StatusBadRequest)
		return
	}
	// Only stream for a pool that actually exists (also guards the exec args).
	known := false
	if pools, err := system.GetAllPools(); err == nil {
		for _, p := range pools {
			if p != nil && p.Name == pool {
				known = true
				break
			}
		}
	}
	if !known {
		http.Error(w, "unknown pool", http.StatusNotFound)
		return
	}

	conn, err := iostatUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// `-H` no headers, `-p` parseable integers; trailing "1" = 1-second interval.
	cmd := exec.Command("sudo", "zpool", "iostat", "-Hp", pool, "1")
	pr, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}

	done := make(chan struct{})
	var once sync.Once
	closeDone := func() { once.Do(func() { close(done) }) }

	// Parse each sample line → JSON message. Columns:
	//   name  alloc  free  read_ops  write_ops  read_bw  write_bw
	go func() {
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			f := strings.Fields(sc.Text())
			if len(f) < 7 {
				continue
			}
			rops, _ := strconv.ParseInt(f[3], 10, 64)
			wops, _ := strconv.ParseInt(f[4], 10, 64)
			rbw, _ := strconv.ParseInt(f[5], 10, 64)
			wbw, _ := strconv.ParseInt(f[6], 10, 64)
			msg := fmt.Sprintf(`{"r_bw":%d,"w_bw":%d,"r_ops":%d,"w_ops":%d}`, rbw, wbw, rops, wops)
			if err := conn.WriteMessage(websocket.TextMessage, []byte(msg)); err != nil {
				break
			}
		}
		closeDone()
	}()

	// Tear the command down as soon as the client goes away.
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				closeDone()
				return
			}
		}
	}()

	<-done
	if cmd.Process != nil {
		cmd.Process.Kill()
	}
}
