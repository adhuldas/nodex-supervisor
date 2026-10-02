package identity

import (
	"fmt"
	"os"
	"strings"
)

// deviceTreeCompatiblePath is overridable for tests.
var deviceTreeCompatiblePath = "/proc/device-tree/compatible"

// isVariscite reports whether the running kernel's device tree identifies
// the board as a Variscite SoM (e.g. DART-MX8M, VAR-SOM-MX8). The
// /proc/device-tree/compatible file is a NUL-separated list of strings,
// most-specific first, standard on ARM device-tree platforms.
func isVariscite() bool {
	b, err := os.ReadFile(deviceTreeCompatiblePath)
	if err != nil {
		return false
	}
	compat := strings.ToLower(strings.ReplaceAll(string(b), "\x00", " "))
	return strings.Contains(compat, "variscite") || strings.Contains(compat, "var-som")
}

// VarisciteProvider will derive identity from a Variscite i.MX8 SoM's
// factory-programmed unique ID (NXP OCOTP fuses, exposed under
// /sys/devices/soc0/soc_uid on i.MX8 or via NXP's caam/OP-TEE-backed unique
// ID API), giving a hardware root of trust that survives any number of
// reflashes because it lives in silicon, not on the storage medium.
//
// This is intentionally not implemented in Phase 1 (QEMU-only scope). It
// exists so the IdentityProvider abstraction is already shaped for the
// planned Variscite hardware target and no core nodexa-agent logic needs to
// change when that provider is implemented.
type VarisciteProvider struct{}

func (p *VarisciteProvider) Name() string { return "variscite" }

func (p *VarisciteProvider) Collect() (HardwareInfo, error) {
	return nil, fmt.Errorf("identity: variscite provider not implemented in this phase (target: nodexa-variscite-*)")
}
