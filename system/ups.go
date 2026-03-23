package system

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"zfsnas/internal/audit"
	"zfsnas/internal/config"
)

// UPSPrereqsInstalled returns true when the nut packages are present.
func UPSPrereqsInstalled() bool {
	_, err1 := exec.LookPath("upsc")
	_, err2 := os.Stat("/usr/sbin/upsd")
	return err1 == nil && err2 == nil
}

// UPSStatus describes the current state of the UPS.
type UPSStatus struct {
	Name          string            `json:"name"`
	RawStatus     string            `json:"raw_status"`
	OnLine        bool              `json:"on_line"`
	OnBattery     bool              `json:"on_battery"`
	LowBattery    bool              `json:"low_battery"`
	ChargePct     *float64          `json:"charge_pct"`
	RuntimeSecs   *int              `json:"runtime_secs"`
	BattVoltage   *float64          `json:"batt_voltage"`
	InputVoltage  *float64          `json:"input_voltage"`
	OutputVoltage *float64          `json:"output_voltage"`
	LoadPct       *float64          `json:"load_pct"`
	TempC         *float64          `json:"temp_c"`
	Model         string            `json:"model"`
	Manufacturer  string            `json:"manufacturer"`
	Serial        string            `json:"serial"`
	AllVars       map[string]string `json:"all_vars"`
}

// QueryUPS runs `upsc <name>` and parses the output into UPSStatus.
func QueryUPS(name string) (*UPSStatus, error) {
	out, err := exec.Command("upsc", name+"@localhost").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("upsc failed: %w — %s", err, strings.TrimSpace(string(out)))
	}

	vars := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		if idx := strings.Index(line, ": "); idx >= 0 {
			k := strings.TrimSpace(line[:idx])
			v := strings.TrimSpace(line[idx+2:])
			vars[k] = v
		}
	}

	s := &UPSStatus{Name: name, AllVars: vars}

	if v, ok := vars["ups.status"]; ok {
		s.RawStatus = v
		s.OnLine = strings.Contains(v, "OL")
		s.OnBattery = strings.Contains(v, "OB")
		s.LowBattery = strings.Contains(v, "LB")
	}
	if v, ok := vars["battery.charge"]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			s.ChargePct = &f
		}
	}
	if v, ok := vars["battery.runtime"]; ok {
		if i, err := strconv.Atoi(v); err == nil {
			s.RuntimeSecs = &i
		}
	}
	if v, ok := vars["battery.voltage"]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			s.BattVoltage = &f
		}
	}
	if v, ok := vars["input.voltage"]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			s.InputVoltage = &f
		}
	}
	if v, ok := vars["output.voltage"]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			s.OutputVoltage = &f
		}
	}
	if v, ok := vars["ups.load"]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			s.LoadPct = &f
		}
	}
	if v, ok := vars["ups.temperature"]; ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			s.TempC = &f
		}
	}
	s.Model = vars["ups.model"]
	s.Manufacturer = vars["ups.mfr"]
	s.Serial = vars["ups.serial"]

	return s, nil
}

// DetectedUPS holds the result of auto-detection.
type DetectedUPS struct {
	Name            string `json:"name"`
	Driver          string `json:"driver"`
	Port            string `json:"port"`
	MonitorPassword string `json:"monitor_password"`
	ScannerOutput   string `json:"scanner_output,omitempty"` // raw nut-scanner output for debugging
}

// generateMonitorPassword returns a random 32-character hex string.
func generateMonitorPassword() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// fallback: use time-based value
		return fmt.Sprintf("%x%x", time.Now().UnixNano(), time.Now().UnixNano()^0xdeadbeef)
	}
	return hex.EncodeToString(b)
}

// DetectAndConfigureUPS writes all NUT daemon infrastructure config files
// (nut.conf, upsd.conf, upsd.users) regardless of whether a UPS is detected,
// then runs nut-scanner to find a device. If found, ups.conf and upsmon.conf
// are written and nut-client is started. nut-server is always started.
//
// existingPassword is reused when provided (e.g. on re-scan); pass "" to
// generate a new one on first install.
//
// Always returns a non-nil *DetectedUPS with MonitorPassword set.
// Name/Driver/Port are empty when no device was found.
func DetectAndConfigureUPS(existingPassword string) (*DetectedUPS, error) {
	password := existingPassword
	if password == "" {
		password = generateMonitorPassword()
	}

	// Always write daemon infrastructure — these don't need a device.
	if err := ConfigureNUTDaemon("", "", "", password); err != nil {
		return &DetectedUPS{MonitorPassword: password}, fmt.Errorf("write NUT daemon config: %w", err)
	}

	// Locate nut-scanner — it may live in /usr/sbin which is not always in sudo PATH.
	scannerBin := "/usr/sbin/nut-scanner"
	if _, err := os.Stat(scannerBin); err != nil {
		if p, err2 := exec.LookPath("nut-scanner"); err2 == nil {
			scannerBin = p
		}
	}

	// -C outputs NUT config-file format (ready for ups.conf).
	// -N skips network protocol scanning (USB UPS scan is instant).
	// Use Output() (stdout only) — nut-scanner writes noise to stderr that must
	// not end up in ups.conf (missing SNMP/XML/IPMI libs, avahi errors, etc.).
	scanOut, _ := nutScannerOutput(scannerBin, "-C", "-N")
	scanRaw := strings.TrimSpace(scanOut)

	// Fallback: try full scan if -C -N produced nothing.
	if !strings.Contains(scanRaw, "[") {
		scanOut2, _ := nutScannerOutput(scannerBin, "-a", "-t", "2")
		if s := strings.TrimSpace(scanOut2); strings.Contains(s, "[") {
			scanRaw = s
		}
	}

	result := &DetectedUPS{MonitorPassword: password, ScannerOutput: scanRaw}

	if strings.Contains(scanRaw, "[") {
		// Write the scanner output directly to ups.conf — no reconstruction needed.
		if err := writeFileViaSudo("/etc/nut/ups.conf", scanRaw+"\n", 0640); err != nil {
			return result, fmt.Errorf("write ups.conf: %w", err)
		}

		// Parse device name from first section header, e.g. [nutdev1].
		result.Name = parseSectionName(scanRaw)
		// Also parse driver/port for display in the UI.
		if d := parseNutScannerOutput(scanRaw); d != nil {
			result.Driver = d.Driver
			result.Port = d.Port
			if result.Name == "" {
				result.Name = d.Name
			}
		}

		if result.Name != "" {
			if err := ApplyUPSMonConfig(result.Name, password); err != nil {
				return result, fmt.Errorf("write upsmon.conf: %w", err)
			}
		}
	}

	fixNUTPermissions()

	// nut-server can start without any devices defined.
	exec.Command("sudo", "systemctl", "enable", "nut-server").Run()
	exec.Command("sudo", "systemctl", "restart", "nut-server").Run()

	// nut-client (upsmon) requires a valid upsmon.conf with a known device.
	if result.Name != "" {
		exec.Command("sudo", "systemctl", "enable", "nut-client").Run()
		exec.Command("sudo", "systemctl", "restart", "nut-client").Run()
	}

	// Brief pause so upsd has time to bind the socket before upsc queries it.
	time.Sleep(2 * time.Second)

	return result, nil
}

// parseSectionName returns the name inside the first [section] header in raw.
func parseSectionName(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			return strings.TrimSpace(line[1 : len(line)-1])
		}
	}
	return ""
}

// parseNutScannerOutput parses INI-style nut-scanner output and returns the
// first device's driver and port. The Name field is left empty; use
// parseSectionName to get the actual device name from the section header.
func parseNutScannerOutput(raw string) *DetectedUPS {
	var current *DetectedUPS
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = &DetectedUPS{}
			continue
		}
		if current == nil {
			continue
		}
		k, v, ok := parseIniKV(line)
		if !ok {
			continue
		}
		switch k {
		case "driver":
			current.Driver = v
		case "port":
			current.Port = v
		}
		if current.Driver != "" && current.Port != "" {
			return current
		}
	}
	return nil
}

// parseIniKV splits "key = value" (with optional quotes on value).
func parseIniKV(line string) (k, v string, ok bool) {
	idx := strings.Index(line, "=")
	if idx < 0 {
		return
	}
	k = strings.TrimSpace(line[:idx])
	v = strings.TrimSpace(line[idx+1:])
	v = strings.Trim(v, `"'`)
	ok = k != ""
	return
}


// ConfigureNUTDaemon writes the NUT daemon infrastructure files:
// nut.conf (MODE=standalone), upsd.conf (LISTEN 127.0.0.1 3493),
// and upsd.users (full-access monitor user with the given password).
// ups.conf is written separately once a device is detected.
func ConfigureNUTDaemon(_, _, _ string, monitorPassword string) error {
	if err := writeFileViaSudo("/etc/nut/nut.conf", "MODE=standalone\n", 0640); err != nil {
		return fmt.Errorf("write nut.conf: %w", err)
	}

	upsdConf := "LISTEN 127.0.0.1 3493\n"
	if err := writeFileViaSudo("/etc/nut/upsd.conf", upsdConf, 0640); err != nil {
		return fmt.Errorf("write upsd.conf: %w", err)
	}

	// Full-access monitor user with generated password.
	upsdUsers := fmt.Sprintf("[upsmon]\n  password  = %s\n  actions   = SET\n  instcmds  = ALL\n  upsmon master\n", monitorPassword)
	if err := writeFileViaSudo("/etc/nut/upsd.users", upsdUsers, 0640); err != nil {
		return fmt.Errorf("write upsd.users: %w", err)
	}

	return nil
}

// ApplyUPSMonConfig writes /etc/nut/upsmon.conf.
// SHUTDOWNCMD is always a no-op — shutdown is managed by StartUPSShutdownWatcher,
// not by NUT, to allow precise control over timing and thresholds.
func ApplyUPSMonConfig(name, monitorPassword string) error {
	monConf := fmt.Sprintf(`# Generated by ZFSNAS — do not edit manually
MONITOR %s@localhost 1 upsmon %s master
SHUTDOWNCMD "/bin/true"
MINSUPPLIES 1
POLLFREQ 5
POLLFREQALERT 5
HOSTSYNC 15
DEADTIME 15
POWERDOWNFLAG /etc/killpower
NOTIFYFLAG ONLINE  SYSLOG+WALL
NOTIFYFLAG ONBATT  SYSLOG+WALL
NOTIFYFLAG LOWBATT SYSLOG+WALL
`, name, monitorPassword)
	if err := writeFileViaSudo("/etc/nut/upsmon.conf", monConf, 0640); err != nil {
		return fmt.Errorf("write upsmon.conf: %w", err)
	}
	return nil
}

// StartUPSShutdownWatcher polls the UPS every 5 seconds and issues a system
// shutdown when ALL of the following conditions are true:
//  1. UPS feature is enabled and a shutdown policy is configured.
//  2. The UPS reports OnBattery=true and OnLine=false (not on AC power).
//  3. The battery-power condition has been confirmed for ≥20 consecutive seconds.
//  4. The configured threshold (charge %, runtime, or both) has been crossed.
//
// NUT's own SHUTDOWNCMD is always "/bin/true" — this goroutine is the sole
// shutdown authority so that threshold logic stays fully in our control.
func StartUPSShutdownWatcher(appCfg *config.AppConfig) {
	const pollInterval = 5 * time.Second
	const onBatteryRequired = 20 * time.Second

	var onBatterySince time.Time
	shutdownFired := false

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for range ticker.C {
		ups := appCfg.UPS
		if !ups.Enabled || ups.UPSName == "" || !ups.ShutdownPolicy.Enabled {
			onBatterySince = time.Time{}
			shutdownFired = false
			continue
		}

		status, err := QueryUPS(ups.UPSName)
		if err != nil {
			// Transient query failure — keep existing battery timer.
			continue
		}

		// Reset when AC power is restored.
		if !status.OnBattery || status.OnLine {
			onBatterySince = time.Time{}
			shutdownFired = false
			continue
		}

		// First poll on battery — record start time and log the transition.
		if onBatterySince.IsZero() {
			onBatterySince = time.Now()
			audit.Log(audit.Entry{
				User:   "system",
				Role:   "system",
				Action: audit.ActionUPSOnBattery,
				Result: audit.ResultOK,
				Target: ups.UPSName,
				Details: fmt.Sprintf("UPS switched to battery power — AC lost; shutdown policy active: %v", ups.ShutdownPolicy.Enabled),
			})
			log.Printf("UPS: AC power lost on %s — now on battery", ups.UPSName)
		}

		// Require 20 s of confirmed battery power before any action.
		if time.Since(onBatterySince) < onBatteryRequired {
			continue
		}

		if shutdownFired {
			continue
		}

		// Evaluate shutdown thresholds.
		policy := ups.ShutdownPolicy
		trigger := false
		switch policy.TriggerType {
		case "percent":
			if status.ChargePct != nil && *status.ChargePct <= float64(policy.PercentThreshold) {
				trigger = true
			}
		case "time":
			if status.RuntimeSecs != nil && *status.RuntimeSecs <= policy.RuntimeThreshold {
				trigger = true
			}
		case "both":
			percentMet := status.ChargePct != nil && *status.ChargePct <= float64(policy.PercentThreshold)
			timeMet := status.RuntimeSecs != nil && *status.RuntimeSecs <= policy.RuntimeThreshold
			trigger = percentMet || timeMet
		}

		if !trigger {
			continue
		}

		shutdownFired = true
		battSecs := int(time.Since(onBatterySince).Seconds())
		details := fmt.Sprintf("on battery for %ds; trigger=%s", battSecs, policy.TriggerType)
		if status.ChargePct != nil {
			details += fmt.Sprintf("; charge=%.0f%%", *status.ChargePct)
		}
		if status.RuntimeSecs != nil {
			details += fmt.Sprintf("; runtime=%ds", *status.RuntimeSecs)
		}
		if policy.PreShutdownCmd != "" {
			details += "; pre-cmd: " + policy.PreShutdownCmd
		}
		audit.Log(audit.Entry{
			User:    "system",
			Role:    "system",
			Action:  audit.ActionUPSShutdown,
			Result:  audit.ResultOK,
			Target:  ups.UPSName,
			Details: details,
		})
		log.Printf("UPS: initiating shutdown — %s", details)

		if policy.PreShutdownCmd != "" {
			exec.Command("sh", "-c", policy.PreShutdownCmd).Run()
		}
		exec.Command("/sbin/shutdown", "-h", "+0").Run()

		// Stop zfsnas gracefully so connections close cleanly before the OS halts.
		syscall.Kill(os.Getpid(), syscall.SIGTERM)
	}
}

// UninstallNUT stops and disables all NUT services, purges the nut packages,
// and removes the /etc/nut configuration directory.
func UninstallNUT() error {
	// Stop and disable services (tolerate errors — they may not be running).
	for _, svc := range []string{"nut-client", "nut-server", "nut-monitor"} {
		exec.Command("sudo", "systemctl", "stop", svc).Run()
		exec.Command("sudo", "systemctl", "disable", svc).Run()
	}

	// Purge packages so no residual config is left by apt.
	if out, err := exec.Command("sudo", "env", "DEBIAN_FRONTEND=noninteractive",
		"apt-get", "purge", "-y", "nut", "nut-client", "nut-server").CombinedOutput(); err != nil {
		// Try plain remove if purge fails (older apt versions).
		if out2, err2 := exec.Command("sudo", "env", "DEBIAN_FRONTEND=noninteractive",
			"apt-get", "remove", "-y", "nut", "nut-client").CombinedOutput(); err2 != nil {
			return fmt.Errorf("apt remove nut: %s", strings.TrimSpace(string(out)+string(out2)))
		}
		_ = out
	}

	// Remove the /etc/nut directory and all config files inside it.
	exec.Command("sudo", "rm", "-rf", "/etc/nut").Run()

	// Remove leftover nut-driver systemd unit fragments that apt purge leaves
	// behind. These include the target.wants directory, the template drop-in,
	// and any per-device drop-in directories (e.g. nut-driver@nutdev1.service.d).
	nutSystemdPaths := []string{
		"/etc/systemd/system/nut-driver.target.wants",
		"/etc/systemd/system/nut-driver@.service.d",
	}
	// Also glob any per-device drop-ins: nut-driver@<name>.service.d
	if entries, err := os.ReadDir("/etc/systemd/system"); err == nil {
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "nut-driver@") && strings.HasSuffix(e.Name(), ".service.d") {
				nutSystemdPaths = append(nutSystemdPaths, "/etc/systemd/system/"+e.Name())
			}
		}
	}
	for _, p := range nutSystemdPaths {
		exec.Command("sudo", "rm", "-rf", p).Run()
	}

	// Reload systemd so removed unit fragments no longer appear in unit state.
	exec.Command("sudo", "systemctl", "daemon-reload").Run()
	exec.Command("sudo", "systemctl", "reset-failed").Run()

	return nil
}

// nutScannerOutput runs nut-scanner via sudo, capturing stdout only.
// stderr is intentionally discarded — nut-scanner writes library-not-found
// warnings and avahi/SNMP/XML/IPMI noise to stderr that must never appear
// in ups.conf.
func nutScannerOutput(bin string, args ...string) (string, error) {
	cmd := exec.Command("sudo", append([]string{bin}, args...)...)
	var stdout strings.Builder
	cmd.Stdout = &stdout
	// cmd.Stderr left nil → discarded
	err := cmd.Run()
	return stdout.String(), err
}

// fixNUTPermissions sets the standard ownership and mode for /etc/nut/ so
// that the nut daemon can read its config files.
func fixNUTPermissions() {
	exec.Command("sudo", "chown", "root:nut", "/etc/nut").Run()
	exec.Command("sudo", "chmod", "750", "/etc/nut").Run()
	for _, f := range []string{
		"/etc/nut/nut.conf",
		"/etc/nut/ups.conf",
		"/etc/nut/upsd.conf",
		"/etc/nut/upsd.users",
		"/etc/nut/upsmon.conf",
	} {
		exec.Command("sudo", "chown", "root:nut", f).Run()
		exec.Command("sudo", "chmod", "640", f).Run()
	}
}

// RestartNUTServices stops then starts nut-server and nut-client in the
// correct order, tolerating failures on distros with different unit names.
func RestartNUTServices() {
	for _, svc := range []string{"nut-client", "nut-server"} {
		exec.Command("sudo", "systemctl", "stop", svc).Run()
	}
	time.Sleep(500 * time.Millisecond)
	for _, svc := range []string{"nut-server", "nut-client"} {
		exec.Command("sudo", "systemctl", "start", svc).Run()
	}
}

// UPSServiceAction runs systemctl start|stop|restart on nut-server and nut-client.
func UPSServiceAction(action string) error {
	var svcs []string
	if action == "stop" {
		// Stop client before server
		svcs = []string{"nut-client", "nut-server"}
	} else {
		// Start/restart server before client
		svcs = []string{"nut-server", "nut-client"}
	}
	var lastErr error
	for _, svc := range svcs {
		if out, err := exec.Command("sudo", "systemctl", action, svc).CombinedOutput(); err != nil {
			lastErr = fmt.Errorf("systemctl %s %s: %s", action, svc, strings.TrimSpace(string(out)))
		}
	}
	return lastErr
}

// writeFileViaSudo writes content to path using sudo tee.
func writeFileViaSudo(path, content string, _ os.FileMode) error {
	cmd := exec.Command("sudo", "tee", path)
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("tee %s: %s", path, strings.TrimSpace(string(out)))
	}
	return nil
}
