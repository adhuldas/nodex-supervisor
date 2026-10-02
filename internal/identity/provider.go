// Package identity implements Nodexa's stable device identity subsystem.
//
//	Hardware Information
//	        |
//	        v
//	Identity Provider
//	        |
//	        v
//	Normalization
//	        |
//	        v
//	Hardware Fingerprint
//	        |
//	        v
//	Deterministic Local Identity
//
// A Provider knows how to collect raw, platform-specific hardware
// information. The Manager normalizes that information, derives a stable
// fingerprint from it, and produces a deterministic "nodexa-device-id" that
// must remain the same across reboot, power cycle, OS update, and repeated
// reflashing of a given logical device.
//
// Providers must never themselves decide the final device ID -- that is the
// Manager's job, so that fingerprinting and normalization rules stay
// consistent no matter which platform collected the raw data. This keeps the
// design portable: QEMU today, generic Linux boards tomorrow, Variscite SoMs
// (i.MX8, etc.) after that, without changing nodexa-agent's core logic.
package identity

// HardwareInfo is the raw, unnormalized set of hardware properties collected
// by a Provider. Keys are provider-defined but should be stable, ASCII,
// lower_snake_case identifiers (e.g. "dmi_product_uuid", "cpu_serial").
//
// Only include values here that are rooted in something physically or
// virtually persistent (firmware tables, SoC fuses, disk/NIC hardware
// identifiers, a pinned virtual-machine UUID). Do not include values that
// live on the writable/reflashable root filesystem (e.g. /etc/machine-id),
// since those must not be relied on for identity that survives reflashing.
type HardwareInfo map[string]string

// Provider collects raw hardware information for a specific platform.
type Provider interface {
	// Name identifies the provider, e.g. "qemu", "linux", "variscite".
	Name() string

	// Collect gathers raw hardware properties. It should return as many
	// stable properties as are available on the running platform; the
	// Manager is responsible for deciding which are sufficient.
	Collect() (HardwareInfo, error)
}

// Detect picks the most specific Provider available on the running system.
// Detection order: Variscite hardware > generic Linux > QEMU virtual
// hardware is intentionally NOT the priority order used; QEMU is checked
// first because its DMI strings are otherwise indistinguishable from a
// generic virtualized Linux host and would be misclassified.
func Detect() Provider {
	if isQEMU() {
		return &QEMUProvider{}
	}
	if isVariscite() {
		return &VarisciteProvider{}
	}
	return &LinuxProvider{}
}

// ForName returns a specific named provider, bypassing auto-detection. Used
// when identity_provider is pinned in configuration (e.g. for testing).
func ForName(name string) Provider {
	switch name {
	case "qemu":
		return &QEMUProvider{}
	case "variscite":
		return &VarisciteProvider{}
	case "linux":
		return &LinuxProvider{}
	default:
		return Detect()
	}
}
