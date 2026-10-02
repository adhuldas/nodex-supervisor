package container

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/events"
)

func TestTransientContainerLifecycle(t *testing.T) {
	bus := events.NewBus(10)
	mgr := NewManager(t.TempDir(), "", t.TempDir(), t.TempDir(), "runc", t.TempDir(), bus)

	var callbackCalls int32
	mgr.SetOnStateChange(func() {
		atomic.AddInt32(&callbackCalls, 1)
	})

	// Initial list must be empty
	list, err := mgr.List()
	if err != nil {
		t.Fatalf("List() err = %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected empty list, got %d", len(list))
	}

	// 1. Pulling image
	mgr.setTransient(NodexaContainer{
		Name:      "web",
		Image:     "nginx:alpine",
		State:     StatePullingImage,
		CreatedAt: time.Now().UTC(),
	})
	if atomic.LoadInt32(&callbackCalls) != 1 {
		t.Fatalf("expected 1 callback call, got %d", callbackCalls)
	}
	list, err = mgr.List()
	if err != nil || len(list) != 1 || list[0].State != StatePullingImage {
		t.Fatalf("expected StatePullingImage, got %v (err: %v)", list, err)
	}

	// 2. Installing image
	mgr.setTransient(NodexaContainer{
		Name:      "web",
		Image:     "nginx:alpine",
		State:     StateInstallingImage,
		CreatedAt: time.Now().UTC(),
	})
	if atomic.LoadInt32(&callbackCalls) != 2 {
		t.Fatalf("expected 2 callback calls, got %d", callbackCalls)
	}
	list, err = mgr.List()
	if err != nil || len(list) != 1 || list[0].State != StateInstallingImage {
		t.Fatalf("expected StateInstallingImage, got %v (err: %v)", list, err)
	}

	// 3. Container created
	mgr.setTransient(NodexaContainer{
		Name:      "web",
		Image:     "nginx:alpine",
		State:     StateContainerCreated,
		CreatedAt: time.Now().UTC(),
	})
	if atomic.LoadInt32(&callbackCalls) != 3 {
		t.Fatalf("expected 3 callback calls, got %d", callbackCalls)
	}
	list, err = mgr.List()
	if err != nil || len(list) != 1 || list[0].State != StateContainerCreated {
		t.Fatalf("expected StateContainerCreated, got %v (err: %v)", list, err)
	}

	// 4. Running / cleared
	mgr.clearTransient("web")
	if atomic.LoadInt32(&callbackCalls) != 4 {
		t.Fatalf("expected 4 callback calls, got %d", callbackCalls)
	}
	list, err = mgr.List()
	if err != nil || len(list) != 0 {
		t.Fatalf("expected empty list after clear, got %v (err: %v)", list, err)
	}
}


func TestWriteBundleConfigEnvOverride(t *testing.T) {
	dir := t.TempDir()
	initialConfig := map[string]any{
		"process": map[string]any{
			"env": []any{
				"PATH=/usr/bin:/bin",
				"EXISTING_VAR=initial_value",
				"RETAIN_VAR=keep_me",
			},
		},
	}
	data, err := json.Marshal(initialConfig)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		t.Fatalf("write config error: %v", err)
	}

	spec := ServiceSpec{
		Name: "test-svc",
		Env: map[string]string{
			"EXISTING_VAR": "overridden_value",
			"NEW_VAR":      "fresh_value",
		},
	}
	volDir := t.TempDir()
	if err := writeBundleConfig(dir, spec, volDir); err != nil {
		t.Fatalf("writeBundleConfig failed: %v", err)
	}

	readData, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("read config error: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(readData, &cfg); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	proc := cfg["process"].(map[string]any)
	rawEnv := proc["env"].([]any)
	envMap := make(map[string]string)
	for _, e := range rawEnv {
		s := e.(string)
		parts := strings.SplitN(s, "=", 2)
		envMap[parts[0]] = parts[1]
	}

	if envMap["EXISTING_VAR"] != "overridden_value" {
		t.Fatalf("expected EXISTING_VAR=overridden_value, got %q", envMap["EXISTING_VAR"])
	}
	if envMap["NEW_VAR"] != "fresh_value" {
		t.Fatalf("expected NEW_VAR=fresh_value, got %q", envMap["NEW_VAR"])
	}
	if envMap["RETAIN_VAR"] != "keep_me" {
		t.Fatalf("expected RETAIN_VAR=keep_me, got %q", envMap["RETAIN_VAR"])
	}
	if len(rawEnv) != 4 {
		t.Fatalf("expected 4 env vars without duplicates, got %d: %v", len(rawEnv), rawEnv)
	}
}

func TestWriteBundleConfigNetworkNamespace(t *testing.T) {
	dir := t.TempDir()
	initialConfig := map[string]any{
		"process": map[string]any{},
		"linux": map[string]any{
			"namespaces": []any{
				map[string]any{"type": "pid"},
				map[string]any{"type": "network"},
			},
		},
	}
	data, err := json.Marshal(initialConfig)
	if err != nil {
		t.Fatalf("marshal error: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		t.Fatalf("write config error: %v", err)
	}

	volDir := t.TempDir()

	// 1. Host network removes network namespace
	specHost := ServiceSpec{Name: "host-svc", Network: "host"}
	if err := writeBundleConfig(dir, specHost, volDir); err != nil {
		t.Fatalf("writeBundleConfig host: %v", err)
	}
	readData, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	var cfgHost map[string]any
	_ = json.Unmarshal(readData, &cfgHost)
	linuxHost := cfgHost["linux"].(map[string]any)
	for _, ns := range linuxHost["namespaces"].([]any) {
		if ns.(map[string]any)["type"] == "network" {
			t.Fatalf("expected network namespace to be removed for host network")
		}
	}

	// 2. Custom network sets network namespace path
	specNet := ServiceSpec{Name: "custom-svc", Network: "my-net"}
	netnsPath := "/run/netns/nodexa-custom-svc"
	if err := writeBundleConfig(dir, specNet, volDir, netnsPath); err != nil {
		t.Fatalf("writeBundleConfig custom net: %v", err)
	}
	readData, _ = os.ReadFile(filepath.Join(dir, "config.json"))
	var cfgNet map[string]any
	_ = json.Unmarshal(readData, &cfgNet)
	linuxNet := cfgNet["linux"].(map[string]any)
	foundNet := false
	for _, ns := range linuxNet["namespaces"].([]any) {
		m := ns.(map[string]any)
		if m["type"] == "network" {
			foundNet = true
			if m["path"] != netnsPath {
				t.Errorf("expected network path %q, got %q", netnsPath, m["path"])
			}
		}
	}
	if !foundNet {
		t.Errorf("expected network namespace to be present for custom network")
	}

	// Verify resolv.conf is written in bundle rootfs
	resolvFile := filepath.Join(dir, "rootfs", "etc", "resolv.conf")
	if _, err := os.Stat(resolvFile); err != nil {
		t.Errorf("expected resolv.conf in rootfs: %v", err)
	}
}


func TestRuncEnvDropsNotifySocket(t *testing.T) {
	got := runcEnv([]string{"PATH=/usr/bin", "NOTIFY_SOCKET=/run/systemd/notify", "HOME=/root"})
	want := []string{"PATH=/usr/bin", "HOME=/root"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("runcEnv = %q, want %q", got, want)
	}
}

// newSeedTestManager returns a manager whose seed dir ships one demo
// bundle, nodexa-test, with containerDir under its own data dir.
func newSeedTestManager(t *testing.T) (*NodexaContainerManager, string) {
	t.Helper()
	seedDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(seedDir, "nodexa-test"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seedDir, "nodexa-test", "metadata.json"), []byte(`{"name":"nodexa-test"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	containerDir := filepath.Join(t.TempDir(), "containers")
	return NewManager(containerDir, seedDir, t.TempDir(), t.TempDir(), "/nonexistent/runc", t.TempDir(), events.NewBus(10)), containerDir
}

func TestRetiredSeedsNeverReturn(t *testing.T) {
	mgr, containerDir := newSeedTestManager(t)
	seeded := filepath.Join(containerDir, "nodexa-test")

	if err := mgr.Bootstrap(); err != nil {
		t.Fatalf("first Bootstrap: %v", err)
	}
	if _, err := os.Stat(seeded); err != nil {
		t.Fatalf("fresh device should be seeded: %v", err)
	}

	if err := mgr.RetireSeeds(nil); err != nil {
		t.Fatalf("RetireSeeds: %v", err)
	}
	if _, err := os.Stat(seeded); !os.IsNotExist(err) {
		t.Fatalf("seeded bundle still present after RetireSeeds: %v", err)
	}

	// A later boot must not copy it back.
	if err := mgr.Bootstrap(); err != nil {
		t.Fatalf("second Bootstrap: %v", err)
	}
	if _, err := os.Stat(seeded); !os.IsNotExist(err) {
		t.Fatal("Bootstrap re-seeded a retired demo container")
	}
	if !mgr.SeedsRetired() {
		t.Fatal("SeedsRetired = false after RetireSeeds")
	}
}

func TestBootstrapRetiresSeedsWhenDeploymentBundleExists(t *testing.T) {
	mgr, containerDir := newSeedTestManager(t)
	bundle := filepath.Join(containerDir, "web")
	if err := os.MkdirAll(bundle, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeMetadata(bundle, &metadata{Name: "web", DeploymentName: "release-1"}); err != nil {
		t.Fatal(err)
	}

	if err := mgr.Bootstrap(); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if _, err := os.Stat(filepath.Join(containerDir, "nodexa-test")); !os.IsNotExist(err) {
		t.Fatal("device with a deployment was re-seeded")
	}
	if !mgr.SeedsRetired() {
		t.Fatal("SeedsRetired = false on a device that already had a deployment")
	}
}

// An agent restart must not forget the containers still running under
// runc: a sibling restarted afterwards has to find them by name and must
// not be handed their IPs.
func TestSetNetworkManagerRestoresRunningContainers(t *testing.T) {
	containerDir := t.TempDir()
	mgr := NewManager(containerDir, "", t.TempDir(), t.TempDir(), "runc", t.TempDir(), nil)
	for name, md := range map[string]*metadata{
		"printer": {Name: "printer", Network: "bridge", IPAddress: "172.17.0.2", Aliases: []string{"smart-printer-firmware"}},
		"nginx":   {Name: "nginx", Network: "bridge", IPAddress: "172.17.0.3"},
		"stopped": {Name: "stopped", Network: "bridge", IPAddress: "172.17.0.4"},
	} {
		dir := filepath.Join(containerDir, name)
		if err := os.MkdirAll(filepath.Join(dir, "rootfs", "etc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeMetadata(dir, md); err != nil {
			t.Fatal(err)
		}
	}

	nm, err := NewNetworkManager(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewNetworkManager: %v", err)
	}
	mgr.netMgr = nm
	mgr.restoreNetworkLocked(func(name string) bool { return name != "stopped" })

	if ip, ok := nm.Resolve("bridge", "smart-printer-firmware"); !ok || ip.String() != "172.17.0.2" {
		t.Fatalf("smart-printer-firmware resolves to %v, %v", ip, ok)
	}
	if _, ok := nm.Resolve("bridge", "stopped"); ok {
		t.Fatalf("a stopped container was registered")
	}
	hosts, _ := os.ReadFile(filepath.Join(containerDir, "nginx", "rootfs", "etc", "hosts"))
	if !strings.Contains(string(hosts), "172.17.0.2 printer smart-printer-firmware") {
		t.Fatalf("nginx /etc/hosts lacks its sibling:\n%s", hosts)
	}

	// nginx restarted: stopped (releasing .3), then started again.
	_ = nm.TeardownContainerNetwork("nginx")
	nm.ReserveContainerIP("nginx", "172.17.0.3")
	bridge, _ := nm.Get("bridge")
	if ip, _ := nm.AllocateContainerIP(bridge, "nginx"); ip != "172.17.0.3" {
		t.Fatalf("restarted nginx got %s, want its old 172.17.0.3", ip)
	}
}

func TestApplyEnvSetsDeviceEnvOverImageEnv(t *testing.T) {
	dir := t.TempDir()
	cfg := `{"process":{"env":["PATH=/bin","DEVICE_ID=stale"]}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := applyEnv(dir, map[string]string{"DEVICE_ID": "ndx_dev_1"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Process struct {
			Env []string `json:"env"`
		} `json:"process"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	want := []string{"PATH=/bin", "DEVICE_ID=ndx_dev_1"}
	if !reflect.DeepEqual(got.Process.Env, want) {
		t.Fatalf("env = %v, want %v", got.Process.Env, want)
	}
}
