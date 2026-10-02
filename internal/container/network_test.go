package container

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNetworkManagerBuiltins(t *testing.T) {
	dir := t.TempDir()
	nm, err := NewNetworkManager(dir, nil)
	if err != nil {
		t.Fatalf("NewNetworkManager: %v", err)
	}

	list := nm.List()
	if len(list) < 3 {
		t.Fatalf("expected at least 3 builtin networks, got %d", len(list))
	}

	names := map[string]bool{}
	for _, n := range list {
		names[n.Name] = true
	}

	for _, req := range []string{"bridge", "host", "none"} {
		if !names[req] {
			t.Errorf("missing builtin network %q", req)
		}
	}
}

func TestNetworkManagerCreateAndRemove(t *testing.T) {
	dir := t.TempDir()
	nm, err := NewNetworkManager(dir, nil)
	if err != nil {
		t.Fatalf("NewNetworkManager: %v", err)
	}

	// Create
	created, err := nm.Create(CreateNetworkRequest{Name: "custom-net"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Name != "custom-net" {
		t.Errorf("expected name custom-net, got %s", created.Name)
	}
	if created.Driver != "bridge" {
		t.Errorf("expected default driver bridge, got %s", created.Driver)
	}
	if created.Subnet == "" || created.Gateway == "" {
		t.Errorf("expected auto-allocated subnet and gateway, got subnet=%s gw=%s", created.Subnet, created.Gateway)
	}

	// Duplicate create should fail
	if _, err := nm.Create(CreateNetworkRequest{Name: "custom-net"}); err == nil {
		t.Errorf("expected error creating duplicate network")
	}

	// Get by Name
	found, err := nm.Get("custom-net")
	if err != nil || found.ID != created.ID {
		t.Fatalf("Get by name failed: %v", err)
	}

	// Get by ID prefix
	foundID, err := nm.Get(created.ID[:6])
	if err != nil || foundID.Name != "custom-net" {
		t.Fatalf("Get by ID prefix failed: %v", err)
	}

	// Predefined removal should fail
	if err := nm.Remove("bridge"); err == nil {
		t.Errorf("expected error removing predefined network bridge")
	}

	// Remove custom
	if err := nm.Remove("custom-net"); err != nil {
		t.Fatalf("Remove custom-net: %v", err)
	}

	// Verify it is gone
	if _, err := nm.Get("custom-net"); err == nil {
		t.Errorf("expected custom-net to be gone after remove")
	}
}

func TestEnsureNetwork(t *testing.T) {
	dir := t.TempDir()
	nm, err := NewNetworkManager(dir, nil)
	if err != nil {
		t.Fatalf("NewNetworkManager: %v", err)
	}

	// 1. Existing builtin network
	br, err := nm.EnsureNetwork("bridge")
	if err != nil {
		t.Fatalf("EnsureNetwork(bridge): %v", err)
	}
	if br.Name != "bridge" {
		t.Errorf("expected bridge, got %s", br.Name)
	}

	// 2. Non-existent network should be auto-created
	autoNet, err := nm.EnsureNetwork("smart-printer-net")
	if err != nil {
		t.Fatalf("EnsureNetwork(smart-printer-net): %v", err)
	}
	if autoNet.Name != "smart-printer-net" {
		t.Errorf("expected smart-printer-net, got %s", autoNet.Name)
	}
	if autoNet.Driver != "bridge" {
		t.Errorf("expected bridge driver, got %s", autoNet.Driver)
	}
	if autoNet.Subnet == "" || autoNet.Gateway == "" {
		t.Errorf("expected subnet and gateway, got %s, %s", autoNet.Subnet, autoNet.Gateway)
	}

	// 3. Ensuring again should return existing without error
	again, err := nm.EnsureNetwork("smart-printer-net")
	if err != nil {
		t.Fatalf("EnsureNetwork existing: %v", err)
	}
	if again.ID != autoNet.ID {
		t.Errorf("expected same ID %s, got %s", autoNet.ID, again.ID)
	}
}

func TestSetupAndTeardownContainerNetwork(t *testing.T) {
	dir := t.TempDir()
	nm, err := NewNetworkManager(dir, nil)
	if err != nil {
		t.Fatalf("NewNetworkManager: %v", err)
	}

	netObj, err := nm.EnsureNetwork("device-net")
	if err != nil {
		t.Fatalf("EnsureNetwork: %v", err)
	}

	netnsPath, ip, err := nm.SetupContainerNetwork("smart-printer-firmware", netObj)
	if err != nil {
		t.Fatalf("SetupContainerNetwork: %v", err)
	}
	if netnsPath != "/run/netns/nodexa-smart-printer-firmware" {
		t.Errorf("unexpected netnsPath: %s", netnsPath)
	}
	if ip == "" {
		t.Errorf("expected non-empty IP address")
	}

	// Allocating another container on the same network should give a different IP
	_, ip2, err := nm.SetupContainerNetwork("sensor-service", netObj)
	if err != nil {
		t.Fatalf("SetupContainerNetwork container 2: %v", err)
	}
	if ip2 == ip {
		t.Errorf("expected distinct IP, got both %s", ip)
	}

	// Teardown
	if err := nm.TeardownContainerNetwork("smart-printer-firmware"); err != nil {
		t.Fatalf("TeardownContainerNetwork: %v", err)
	}
	if err := nm.TeardownContainerNetwork("sensor-service"); err != nil {
		t.Fatalf("TeardownContainerNetwork: %v", err)
	}
}

func TestSyncNetworkHosts(t *testing.T) {
	dir := t.TempDir()
	nm, err := NewNetworkManager(dir, nil)
	if err != nil {
		t.Fatalf("NewNetworkManager: %v", err)
	}

	containersDir := t.TempDir()

	// Setup container 1: nginx
	nginxEtc := filepath.Join(containersDir, "nginx", "rootfs", "etc")
	if err := os.MkdirAll(nginxEtc, 0o755); err != nil {
		t.Fatalf("mkdir nginx: %v", err)
	}

	// Setup container 2: smart-printer-firmware
	printerEtc := filepath.Join(containersDir, "smart-printer-firmware", "rootfs", "etc")
	if err := os.MkdirAll(printerEtc, 0o755); err != nil {
		t.Fatalf("mkdir printer: %v", err)
	}

	// Register both on bridge
	nm.RegisterContainer("bridge", "nginx", "172.17.0.2", nil, []ContainerPort{{ContainerPort: 80, HostPort: 80, Protocol: "tcp"}})
	nm.RegisterContainer("bridge", "smart-printer-firmware", "172.17.0.3", []string{"printer-alias"}, nil)

	// Sync hosts
	nm.SyncNetworkHosts("bridge", containersDir)

	// Verify nginx /etc/hosts
	nginxHosts, err := os.ReadFile(filepath.Join(nginxEtc, "hosts"))
	if err != nil {
		t.Fatalf("read nginx hosts: %v", err)
	}
	sNginx := string(nginxHosts)
	if !strings.Contains(sNginx, "172.17.0.2 nginx") {
		t.Errorf("expected 172.17.0.2 nginx in nginx hosts, got:\n%s", sNginx)
	}
	if !strings.Contains(sNginx, "172.17.0.3 smart-printer-firmware printer-alias") {
		t.Errorf("expected 172.17.0.3 smart-printer-firmware in nginx hosts, got:\n%s", sNginx)
	}
	if strings.Contains(sNginx, "127.0.0.1 smart-printer-firmware") {
		t.Errorf("127.0.0.1 smart-printer-firmware MUST NOT be present in hosts file")
	}

	// Verify smart-printer-firmware /etc/hosts
	printerHosts, err := os.ReadFile(filepath.Join(printerEtc, "hosts"))
	if err != nil {
		t.Fatalf("read printer hosts: %v", err)
	}
	sPrinter := string(printerHosts)
	if !strings.Contains(sPrinter, "172.17.0.3 smart-printer-firmware printer-alias") {
		t.Errorf("expected 172.17.0.3 smart-printer-firmware in printer hosts, got:\n%s", sPrinter)
	}
	if !strings.Contains(sPrinter, "172.17.0.2 nginx") {
		t.Errorf("expected 172.17.0.2 nginx in printer hosts, got:\n%s", sPrinter)
	}

	// Unregister printer and re-sync
	nm.UnregisterContainer("smart-printer-firmware")
	nm.SyncNetworkHosts("bridge", containersDir)

	nginxHostsAfter, _ := os.ReadFile(filepath.Join(nginxEtc, "hosts"))
	if strings.Contains(string(nginxHostsAfter), "smart-printer-firmware") {
		t.Errorf("expected smart-printer-firmware to be removed from nginx hosts after unregister")
	}
}

func TestIsNetnsMountRejectsPlainFile(t *testing.T) {
	// A leftover /run/netns entry whose mount lives in another mount
	// namespace looks exactly like this: an empty regular file.
	p := filepath.Join(t.TempDir(), "nodexa-web")
	if err := os.WriteFile(p, nil, 0o444); err != nil {
		t.Fatal(err)
	}
	if isNetnsMount(p) {
		t.Fatalf("isNetnsMount(%q) = true for a plain file", p)
	}
	if isNetnsMount(p + "-missing") {
		t.Fatal("isNetnsMount = true for a missing path")
	}
}

func TestReserveContainerIP(t *testing.T) {
	nm, err := NewNetworkManager(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewNetworkManager: %v", err)
	}
	bridge, _ := nm.Get("bridge")

	if !nm.ReserveContainerIP("api", "172.17.0.2") {
		t.Fatalf("reserving a free IP failed")
	}
	if nm.ReserveContainerIP("web", "172.17.0.2") {
		t.Fatalf("reserved an IP another container holds")
	}
	// The next allocation must skip the reserved IP...
	if ip, _ := nm.AllocateContainerIP(bridge, "web"); ip != "172.17.0.3" {
		t.Fatalf("web got %s, want 172.17.0.3", ip)
	}
	// ...and a container with a reservation gets it back.
	if ip, _ := nm.AllocateContainerIP(bridge, "api"); ip != "172.17.0.2" {
		t.Fatalf("api got %s, want its reserved 172.17.0.2", ip)
	}
}
