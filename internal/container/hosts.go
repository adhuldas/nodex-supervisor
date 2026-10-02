package container

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// syncHostNetworkHostsLocked writes every host-network container's
// /etc/hosts, mapping the names (and container_name aliases) of all
// host-network containers to 127.0.0.1. They share the device's network
// stack, so a sibling is reached on loopback at its own port -- the same
// compose file that says `proxy_pass http://smart-printer-firmware:9020`
// works unchanged. Without this their /etc/hosts holds only localhost:
// the bridge network's DNS server and SyncNetworkHosts cover bridge
// containers only. The caller holds m.mu.
func (m *NodexaContainerManager) syncHostNetworkHostsLocked() {
	entries, err := os.ReadDir(m.containerDir)
	if err != nil {
		return
	}
	var bundles []string
	names := map[string]bool{}
	for _, e := range entries {
		md, err := readMetadata(filepath.Join(m.containerDir, e.Name()))
		if err != nil || md.Network != "host" {
			continue
		}
		bundles = append(bundles, e.Name())
		names[e.Name()] = true
		for _, a := range md.Aliases {
			names[a] = true
		}
	}
	delete(names, "localhost")
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	var b strings.Builder
	b.WriteString("127.0.0.1 localhost localhost.localdomain\n")
	b.WriteString("::1 localhost localhost.localdomain\n")
	if len(sorted) > 0 {
		b.WriteString("\n# Host-network containers\n127.0.0.1 " + strings.Join(sorted, " ") + "\n")
	}
	for _, name := range bundles {
		etcDir := filepath.Join(m.containerDir, name, "rootfs", "etc")
		if fi, err := os.Stat(etcDir); err != nil || !fi.IsDir() {
			continue
		}
		_ = os.WriteFile(filepath.Join(etcDir, "hosts"), []byte(b.String()), 0o644)
	}
}
