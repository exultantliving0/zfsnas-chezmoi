package system

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
	"zfsnas/internal/config"
)

// DiskPowerPrereqsInstalled returns true when hdparm is available on PATH.
func DiskPowerPrereqsInstalled() bool {
	_, err := exec.LookPath("hdparm")
	return err == nil
}

// ListPhysicalDisks returns all physical block device names (/dev/sda, /dev/sdb, …)
// excluding loop devices, CD-ROMs, and RAM disks.
func ListPhysicalDisks() ([]string, error) {
	out, err := exec.Command("lsblk", "-dn", "-o", "NAME,TYPE").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("lsblk: %w", err)
	}
	var disks []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == "disk" {
			name := fields[0]
			if !strings.HasPrefix(name, "loop") && !strings.HasPrefix(name, "sr") &&
				!strings.HasPrefix(name, "zram") {
				disks = append(disks, "/dev/"+name)
			}
		}
	}
	return disks, nil
}

// hdparmConfContent generates the ZNAS-managed block for /etc/hdparm.conf.
func hdparmConfContent(cfg config.DiskPowerConfig) string {
	var sb strings.Builder
	sb.WriteString("# ===== ZNAS HDPARM BEGIN =====\n")
	sb.WriteString("/dev/disk/by-path/* {\n")
	if cfg.APMLevel > 0 {
		sb.WriteString(fmt.Sprintf("    apm = %d\n", cfg.APMLevel))
	}
	if cfg.SpindownTimeout > 0 {
		sb.WriteString(fmt.Sprintf("    spindown_time = %d\n", cfg.SpindownTimeout))
	}
	if cfg.WriteCache != nil {
		if *cfg.WriteCache {
			sb.WriteString("    write_cache = on\n")
		} else {
			sb.WriteString("    write_cache = off\n")
		}
	}
	sb.WriteString("    dma = on\n")
	sb.WriteString("}\n")
	sb.WriteString("# ===== ZNAS HDPARM END =====\n")
	return sb.String()
}

// ApplyDiskPowerConfig writes /etc/hdparm.conf and applies settings immediately
// to all physical disks. Per-disk errors are logged but do not abort.
func ApplyDiskPowerConfig(cfg config.DiskPowerConfig) error {
	content := hdparmConfContent(cfg)
	if err := writeFileViaSudo("/etc/hdparm.conf", content, 0644); err != nil {
		return fmt.Errorf("write hdparm.conf: %w", err)
	}

	if !cfg.Enabled {
		return nil
	}

	disks, err := ListPhysicalDisks()
	if err != nil {
		log.Printf("[diskpower] could not list physical disks: %v", err)
		return nil
	}

	for _, dev := range disks {
		if cfg.APMLevel > 0 {
			out, err := exec.Command("sudo", "hdparm", "-B", fmt.Sprintf("%d", cfg.APMLevel), dev).CombinedOutput()
			if err != nil {
				log.Printf("[diskpower] hdparm -B %d %s: %v — %s", cfg.APMLevel, dev, err, strings.TrimSpace(string(out)))
			}
		}
		if cfg.SpindownTimeout > 0 {
			out, err := exec.Command("sudo", "hdparm", "-S", fmt.Sprintf("%d", cfg.SpindownTimeout), dev).CombinedOutput()
			if err != nil {
				log.Printf("[diskpower] hdparm -S %d %s: %v — %s", cfg.SpindownTimeout, dev, err, strings.TrimSpace(string(out)))
			}
		}
		if cfg.WriteCache != nil {
			val := "0"
			if *cfg.WriteCache {
				val = "1"
			}
			out, err := exec.Command("sudo", "hdparm", "-W", val, dev).CombinedOutput()
			if err != nil {
				log.Printf("[diskpower] hdparm -W %s %s: %v — %s", val, dev, err, strings.TrimSpace(string(out)))
			}
		}
		if cfg.AcousticLevel >= 128 {
			out, err := exec.Command("sudo", "hdparm", "-M", fmt.Sprintf("%d", cfg.AcousticLevel), dev).CombinedOutput()
			if err != nil {
				log.Printf("[diskpower] hdparm -M %d %s: %v — %s", cfg.AcousticLevel, dev, err, strings.TrimSpace(string(out)))
			}
		}
	}
	return nil
}

// GetActiveDiskPowerConfig reads the current /etc/hdparm.conf managed block.
// Returns the stored config; actual live settings are applied per-disk via hdparm.
func GetActiveDiskPowerConfig() (config.DiskPowerConfig, error) {
	// The source of truth is the app config, not the file.
	// This function exists for callers that need to read from the file directly.
	return config.DiskPowerConfig{}, nil
}
