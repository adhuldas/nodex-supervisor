package container

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncHostNetworkHosts(t *testing.T) {
	containerDir := t.TempDir()
	mgr := NewManager(containerDir, "", t.TempDir(), t.TempDir(), "runc", t.TempDir(), nil)
	for name, md := range map[string]*metadata{
		"nginx":                  {Name: "nginx", Network: "host"},
		"smart-printer-firmware": {Name: "smart-printer-firmware", Network: "host", Aliases: []string{"printer"}},
		"bridged":                {Name: "bridged", Network: "bridge", IPAddress: "172.17.0.2"},
	} {
		dir := filepath.Join(containerDir, name)
		if err := os.MkdirAll(filepath.Join(dir, "rootfs", "etc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeMetadata(dir, md); err != nil {
			t.Fatal(err)
		}
	}

	mgr.syncHostNetworkHostsLocked()

	hosts, _ := os.ReadFile(filepath.Join(containerDir, "nginx", "rootfs", "etc", "hosts"))
	if !strings.Contains(string(hosts), "127.0.0.1 nginx printer smart-printer-firmware\n") {
		t.Fatalf("nginx /etc/hosts:\n%s", hosts)
	}
	if b, _ := os.ReadFile(filepath.Join(containerDir, "bridged", "rootfs", "etc", "hosts")); len(b) != 0 {
		t.Fatalf("bridge container's /etc/hosts was written:\n%s", b)
	}
}
