package gsm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Configurable command paths and directories for unit testing.
var (
	mmcliPath = "/usr/bin/mmcli"
	nmcliPath = "/usr/bin/nmcli"
	sysNetDir = "/sys/class/net"
	sysWwanDir = "/sys/class/wwan"
)

// Available checks if cellular/GSM hardware (modem) is supported/present on the system.
func Available(ctx context.Context) bool {
	// 1. Check ModemManager via mmcli -L
	if lookPath(mmcliPath) {
		if out, err := exec.CommandContext(ctx, mmcliPath, "-L").Output(); err == nil {
			str := string(out)
			if strings.Contains(str, "/Modem/") || strings.Contains(str, "/modem/") {
				return true
			}
		}
	}

	// 2. Check NetworkManager via nmcli -t -f TYPE dev
	if lookPath(nmcliPath) {
		if out, err := exec.CommandContext(ctx, nmcliPath, "-t", "-f", "TYPE", "dev").Output(); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				t := strings.TrimSpace(line)
				if t == "gsm" || t == "cdma" || t == "cellular" {
					return true
				}
			}
		}
	}

	// 3. Check Linux kernel /sys/class/wwan
	if entries, err := os.ReadDir(sysWwanDir); err == nil && len(entries) > 0 {
		return true
	}

	// 4. Check /sys/class/net for wwan interfaces or drivers
	if entries, err := os.ReadDir(sysNetDir); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, "wwan") || strings.HasPrefix(name, "wwp") || strings.HasPrefix(name, "cdc-wdm") {
				return true
			}
			driverPath := filepath.Join(sysNetDir, name, "device", "driver")
			if target, err := os.Readlink(driverPath); err == nil {
				driver := filepath.Base(target)
				if isCellularDriver(driver) {
					return true
				}
			}
		}
	}

	return false
}

// Connected checks if a cellular/GSM network connection is currently active and online.
func Connected(ctx context.Context) bool {
	// 1. Check NetworkManager device states via nmcli -t -f TYPE,STATE dev
	if lookPath(nmcliPath) {
		if out, err := exec.CommandContext(ctx, nmcliPath, "-t", "-f", "TYPE,STATE", "dev").Output(); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				fields := strings.Split(strings.TrimSpace(line), ":")
				if len(fields) >= 2 {
					t := fields[0]
					state := fields[1]
					if (t == "gsm" || t == "cdma" || t == "cellular") && state == "connected" {
						return true
					}
				}
			}
		}
	}

	// 2. Check ModemManager via mmcli -m any
	if lookPath(mmcliPath) {
		if out, err := exec.CommandContext(ctx, mmcliPath, "-m", "any").Output(); err == nil {
			str := string(out)
			if strings.Contains(str, "state: connected") || strings.Contains(str, "state: 'connected'") {
				return true
			}
		}
	}

	// 3. Check sysfs network interfaces for operational wwan link
	if entries, err := os.ReadDir(sysNetDir); err == nil {
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, "wwan") || strings.HasPrefix(name, "wwp") {
				operstate, err := os.ReadFile(filepath.Join(sysNetDir, name, "operstate"))
				if err == nil && strings.TrimSpace(string(operstate)) == "up" {
					return true
				}
			}
		}
	}

	return false
}

func lookPath(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

func isCellularDriver(driver string) bool {
	switch driver {
	case "qmi_wwan", "cdc_mbim", "sierra_net", "huawei_cdc_ncm", "option", "cdc_ether":
		return true
	default:
		return false
	}
}
