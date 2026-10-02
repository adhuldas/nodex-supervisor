package identity

import (
	"bufio"
	"os"
	"strings"
)

// procCPUInfoPath is overridable for tests.
var procCPUInfoPath = "/proc/cpuinfo"

// sysBlockRoot is overridable for tests.
var sysBlockRoot = "/sys/block"

// LinuxProvider is the generic fallback identity provider for any Linux
// target that is neither detected as QEMU nor as a known Variscite SoM. It
// is the provider a future, not-yet-modeled board will use until a
// dedicated provider is written for it, so it favors hardware sources that
// are broadly available across ARM and x86 SBCs.
type LinuxProvider struct{}

func (p *LinuxProvider) Name() string { return "linux" }

func (p *LinuxProvider) Collect() (HardwareInfo, error) {
	info := HardwareInfo{}

	if v := readSysFile(dmiRoot + "/product_uuid"); v != "" {
		info["dmi_product_uuid"] = v
	}
	if v := cpuSerial(); v != "" {
		info["cpu_serial"] = v
	}
	if v := primaryBlockDeviceSerial(); v != "" {
		info["block_device_serial"] = v
	}
	if v := readFirstNIC(sysClassNetRoot); v != "" {
		info["primary_nic_mac"] = v
	}

	return info, nil
}

// cpuSerial reads the "Serial" field from /proc/cpuinfo, present on many
// ARM SBCs (e.g. Raspberry Pi) that expose a SoC-unique serial number there.
func cpuSerial() string {
	f, err := os.Open(procCPUInfoPath)
	if err != nil {
		return ""
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Serial") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				serial := strings.TrimSpace(parts[1])
				if serial != "" && !allZero(serial) {
					return serial
				}
			}
		}
	}
	return ""
}

func allZero(s string) bool {
	for _, c := range s {
		if c != '0' {
			return false
		}
	}
	return true
}

// primaryBlockDeviceSerial returns the hardware serial of the first
// non-loopback, non-virtual block device (e.g. mmcblk0, nvme0n1, sda),
// chosen deterministically by name.
func primaryBlockDeviceSerial() string {
	entries, err := os.ReadDir(sysBlockRoot)
	if err != nil {
		return ""
	}

	var candidates []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, "loop") || strings.HasPrefix(name, "ram") || strings.HasPrefix(name, "zram") {
			continue
		}
		candidates = append(candidates, name)
	}
	if len(candidates) == 0 {
		return ""
	}
	first := candidates[0]
	for _, c := range candidates {
		if c < first {
			first = c
		}
	}

	return readSysFile(sysBlockRoot + "/" + first + "/device/serial")
}
