package identity

import (
	"os"
	"path/filepath"
	"strings"
)

// readSysFile reads a sysfs/procfs style single-value file and returns its
// trimmed contents, or "" if it cannot be read. These files are frequently
// unreadable by unprivileged users (e.g. DMI product_uuid on some distros),
// or absent entirely on a given platform -- both are treated as "no data"
// rather than fatal errors, since a Provider should degrade gracefully and
// let the Manager work with whatever subset of properties is available.
func readSysFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// readFirstNIC returns the permanent hardware MAC address of the first
// non-loopback, non-virtual network interface it finds under
// /sys/class/net, preferring interfaces with a "device" symlink (i.e. real
// or paravirtualized NICs backed by an actual device, as opposed to bridges,
// veth pairs, or tunnels).
func readFirstNIC(sysClassNet string) string {
	entries, err := os.ReadDir(sysClassNet)
	if err != nil {
		return ""
	}

	var candidates []string
	for _, e := range entries {
		name := e.Name()
		if name == "lo" {
			continue
		}
		if _, err := os.Lstat(filepath.Join(sysClassNet, name, "device")); err != nil {
			continue // skip virtual-only interfaces (bridges, veth, tun/tap)
		}
		candidates = append(candidates, name)
	}
	if len(candidates) == 0 {
		return ""
	}

	// Deterministic: always prefer the lexicographically first real NIC name.
	first := candidates[0]
	for _, c := range candidates {
		if c < first {
			first = c
		}
	}

	addr := readSysFile(filepath.Join(sysClassNet, first, "address"))
	return addr
}

// dirExists reports whether path exists and is a directory.
func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}
