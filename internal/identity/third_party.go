package identity

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Collect gathers raw hardware properties on third-party systems across
// Linux, macOS (Darwin), Windows, and other platforms.
func (p *ThirdPartyProvider) Collect() (HardwareInfo, error) {
	info := HardwareInfo{}

	switch runtime.GOOS {
	case "darwin":
		collectDarwin(info)
	case "windows":
		collectWindows(info)
	case "linux":
		collectLinux(p, info)
	default:
		collectLinux(p, info)
	}

	// Cross-platform fallback for primary_nic_mac if still empty
	if info["primary_nic_mac"] == "" {
		if mac := fallbackFirstNIC(); mac != "" {
			info["primary_nic_mac"] = mac
		}
	}

	// Last-resort fallback so third-party systems never fail derive()
	if len(info) == 0 {
		if host, err := os.Hostname(); err == nil && strings.TrimSpace(host) != "" {
			info["hostname"] = strings.TrimSpace(host)
		}
	}

	return info, nil
}

func collectLinux(p *ThirdPartyProvider, info HardwareInfo) {
	linuxInfo, _ := p.LinuxProvider.Collect()
	for k, v := range linuxInfo {
		if v != "" {
			info[k] = v
		}
	}

	// Container / minimal Linux fallback if /sys/class/dmi/id was missing
	if info["dmi_product_uuid"] == "" {
		if id := readSysFile("/etc/machine-id"); id != "" {
			info["machine_id"] = id
		} else if id := readSysFile("/var/lib/dbus/machine-id"); id != "" {
			info["machine_id"] = id
		}
	}
}

func collectDarwin(info HardwareInfo) {
	// 1. Hardware UUID & Serial via ioreg
	ioregPaths := []string{"/usr/sbin/ioreg", "ioreg"}
	for _, p := range ioregPaths {
		out, err := exec.Command(p, "-rd1", "-c", "IOPlatformExpertDevice").Output()
		if err == nil {
			parseDarwinIOReg(string(out), info)
			break
		}
	}

	// 2. Hardware MAC address via networksetup (retrieves permanent burned-in MAC)
	netsetupPaths := []string{"/usr/sbin/networksetup", "networksetup"}
	for _, p := range netsetupPaths {
		out, err := exec.Command(p, "-listallhardwareports").Output()
		if err == nil {
			if mac := parseDarwinNetworkSetup(string(out)); mac != "" {
				info["primary_nic_mac"] = mac
				break
			}
		}
	}
}

func parseDarwinIOReg(out string, info HardwareInfo) {
	scanner := bufio.NewScanner(strings.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.Contains(line, `"IOPlatformUUID"`) {
			if val := extractQuotedValue(line); val != "" {
				info["platform_uuid"] = val
			}
		}
		if strings.Contains(line, `"IOPlatformSerialNumber"`) {
			if val := extractQuotedValue(line); val != "" {
				info["platform_serial"] = val
			}
		}
	}
}

func extractQuotedValue(line string) string {
	parts := strings.SplitN(line, "=", 2)
	if len(parts) < 2 {
		return ""
	}
	val := strings.TrimSpace(parts[1])
	val = strings.Trim(val, `"`)
	return strings.TrimSpace(val)
}

func parseDarwinNetworkSetup(out string) string {
	scanner := bufio.NewScanner(strings.NewReader(out))
	var currentDevice string
	ports := make(map[string]string)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "Device:") {
			currentDevice = strings.TrimSpace(strings.TrimPrefix(line, "Device:"))
		} else if strings.HasPrefix(line, "Ethernet Address:") {
			mac := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(line, "Ethernet Address:")))
			if currentDevice != "" && isValidMAC(mac) {
				ports[currentDevice] = mac
			}
		} else if line == "" {
			currentDevice = ""
		}
	}

	// en0 is the primary built-in interface on macOS (Ethernet or Wi-Fi)
	if mac, ok := ports["en0"]; ok {
		return mac
	}

	for i := 1; i <= 9; i++ {
		dev := fmt.Sprintf("en%d", i)
		if mac, ok := ports[dev]; ok {
			return mac
		}
	}

	for _, mac := range ports {
		return mac
	}

	return ""
}

func collectWindows(info HardwareInfo) {
	// 1. MachineGuid from registry via reg query
	out, err := exec.Command("reg", "query", `HKLM\SOFTWARE\Microsoft\Cryptography`, "/v", "MachineGuid").Output()
	if err == nil {
		scanner := bufio.NewScanner(strings.NewReader(string(out)))
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if strings.Contains(line, "MachineGuid") {
				fields := strings.Fields(line)
				if len(fields) >= 3 {
					info["platform_uuid"] = fields[len(fields)-1]
					break
				}
			}
		}
	}

	// 2. Fallback to WMIC BIOS UUID
	if info["platform_uuid"] == "" {
		wmicOut, err := exec.Command("wmic", "csproduct", "get", "uuid").Output()
		if err == nil {
			scanner := bufio.NewScanner(strings.NewReader(string(wmicOut)))
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line != "" && !strings.EqualFold(line, "uuid") {
					info["platform_uuid"] = line
					break
				}
			}
		}
	}
}

func isValidMAC(mac string) bool {
	hwMAC, err := net.ParseMAC(mac)
	if err != nil || len(hwMAC) != 6 || bytes.Equal(hwMAC, make(net.HardwareAddr, 6)) {
		return false
	}
	return true
}

func fallbackFirstNIC() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}

	var candidates []net.Interface
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagPointToPoint != 0 {
			continue
		}
		if len(iface.HardwareAddr) != 6 || bytes.Equal(iface.HardwareAddr, make(net.HardwareAddr, 6)) {
			continue
		}
		name := strings.ToLower(iface.Name)
		if strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "veth") ||
			strings.HasPrefix(name, "br-") || strings.HasPrefix(name, "tailscale") ||
			strings.HasPrefix(name, "utun") || strings.HasPrefix(name, "awdl") ||
			strings.HasPrefix(name, "llw") || strings.HasPrefix(name, "anpi") {
			continue
		}
		candidates = append(candidates, iface)
	}

	if len(candidates) == 0 {
		return ""
	}

	var best net.Interface
	var found bool
	for _, iface := range candidates {
		isGlobal := iface.HardwareAddr[0]&0x02 == 0
		if !found {
			best = iface
			found = true
			continue
		}
		bestIsGlobal := best.HardwareAddr[0]&0x02 == 0
		if isGlobal && !bestIsGlobal {
			best = iface
		} else if isGlobal == bestIsGlobal {
			if iface.Name == "en0" || iface.Name == "eth0" {
				best = iface
			} else if best.Name != "en0" && best.Name != "eth0" && iface.Name < best.Name {
				best = iface
			}
		}
	}

	if found {
		return strings.ToLower(best.HardwareAddr.String())
	}
	return ""
}
