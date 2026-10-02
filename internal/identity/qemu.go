package identity

import "strings"

// dmiRoot is a variable (not a const) so tests can point it at a fixture
// directory instead of the real /sys/class/dmi/id.
var dmiRoot = "/sys/class/dmi/id"

// sysClassNetRoot is likewise overridable for tests.
var sysClassNetRoot = "/sys/class/net"

// isQEMU reports whether the running kernel looks like it is inside a QEMU
// virtual machine, based on DMI strings QEMU's firmware (SeaBIOS/OVMF/EDK2)
// populates by default.
func isQEMU() bool {
	vendor := strings.ToLower(readSysFile(dmiRoot + "/sys_vendor"))
	product := strings.ToLower(readSysFile(dmiRoot + "/product_name"))
	return strings.Contains(vendor, "qemu") || strings.Contains(product, "qemu") ||
		strings.Contains(product, "standard pc")
}

// QEMUProvider derives a stable identity for a Nodexa OS instance running
// under QEMU. Because QEMU only reports a stable SMBIOS product_uuid when
// the host explicitly passes one at launch, scripts/run-qemu.sh generates
// and caches a per-machine UUID on first run and passes it via `-uuid` on
// every subsequent run. This intentionally mirrors the real-hardware case:
// a physical SoM has a UUID burned into silicon/fuses at manufacturing time
// that never changes; a QEMU VM has one pinned by the developer's harness
// that plays the same role for testing.
type QEMUProvider struct{}

func (p *QEMUProvider) Name() string { return "qemu" }

func (p *QEMUProvider) Collect() (HardwareInfo, error) {
	info := HardwareInfo{}

	if v := readSysFile(dmiRoot + "/product_uuid"); v != "" {
		info["dmi_product_uuid"] = v
	}
	if v := readSysFile(dmiRoot + "/board_serial"); v != "" {
		info["dmi_board_serial"] = v
	}
	if v := readSysFile(dmiRoot + "/product_serial"); v != "" {
		info["dmi_product_serial"] = v
	}
	if v := readFirstNIC(sysClassNetRoot); v != "" {
		info["primary_nic_mac"] = v
	}

	return info, nil
}
