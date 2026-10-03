package wifi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyfileWPA(t *testing.T) {
	got := Keyfile(Credentials{SSID: "Home;Net", Password: `pa ss\word`})
	for _, want := range []string{
		"id=nodexa-wifi\n",
		"type=wifi\n",
		"ssid=72;111;109;101;59;78;101;116;\n",
		"key-mgmt=wpa-psk\n",
		`psk=pa\sss\\word` + "\n",
		"route-metric=2048\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("keyfile missing %q:\n%s", want, got)
		}
	}
}

func TestKeyfileOpenNetwork(t *testing.T) {
	got := Keyfile(Credentials{SSID: "Cafe"})
	if strings.Contains(got, "[wifi-security]") {
		t.Errorf("open network got a security section:\n%s", got)
	}
}

func TestKeyfileUUIDStable(t *testing.T) {
	a := connectionUUID("Home")
	if a != connectionUUID("Home") {
		t.Fatal("uuid not stable")
	}
	if a == connectionUUID("Other") {
		t.Fatal("different SSIDs share a uuid")
	}
	if len(a) != 36 || a[14] != '5' {
		t.Fatalf("not a version 5 uuid: %s", a)
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		c  Credentials
		ok bool
	}{
		{Credentials{SSID: "Home", Password: "12345678"}, true},
		{Credentials{SSID: "Home"}, true},
		{Credentials{SSID: ""}, false},
		{Credentials{SSID: strings.Repeat("x", 33)}, false},
		{Credentials{SSID: "Home", Password: "short"}, false},
		{Credentials{SSID: "Home", Password: "1234567\n8"}, false},
	} {
		if err := tc.c.Validate(); (err == nil) != tc.ok {
			t.Errorf("Validate(%+v) = %v, want ok=%v", tc.c, err, tc.ok)
		}
	}
}

func TestApplyWritesOnlyOnChange(t *testing.T) {
	nmcliPath = "nodexa-test-no-such-nmcli"
	dir := filepath.Join(t.TempDir(), "system-connections")
	c := Credentials{SSID: "Home", Password: "12345678"}

	changed, err := Apply(context.Background(), dir, c)
	if err != nil || !changed {
		t.Fatalf("first Apply = %v, %v", changed, err)
	}
	path := filepath.Join(dir, "nodexa-wifi.nmconnection")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("keyfile mode %v, want 0600", info.Mode().Perm())
	}

	if changed, err := Apply(context.Background(), dir, c); err != nil || changed {
		t.Fatalf("repeat Apply = %v, %v, want unchanged", changed, err)
	}

	if err := Remove(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("keyfile still present: %v", err)
	}
	if err := Remove(context.Background(), dir); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
}

func TestApplyRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	if _, err := Apply(context.Background(), dir, Credentials{SSID: "Home", Password: "short"}); err == nil {
		t.Fatal("expected error")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("wrote files for invalid credentials: %v", entries)
	}
}

func TestParseScanOutput(t *testing.T) {
	raw := "HomeNetwork:85:WPA2\n" +
		"Office\\:WiFi:60:WPA2 WPA3\n" +
		"GuestNetwork:40:--\n" +
		"HomeNetwork:70:WPA2\n" + // duplicate SSID with weaker signal, should be deduplicated
		"--:90:WPA2\n" + // hidden/empty SSID, should be skipped
		":50:WPA2\n"

	nets := ParseScanOutput(raw)
	if len(nets) != 3 {
		t.Fatalf("expected 3 networks, got %d: %+v", len(nets), nets)
	}

	// Should be sorted by SignalPercent descending: 85, 60, 40
	if nets[0].SSID != "HomeNetwork" || nets[0].SignalPercent != 85 || nets[0].Security != "wpa2-psk" {
		t.Errorf("unexpected first network: %+v", nets[0])
	}
	if nets[1].SSID != "Office:WiFi" || nets[1].SignalPercent != 60 || nets[1].Security != "wpa3-psk" {
		t.Errorf("unexpected second network: %+v", nets[1])
	}
	if nets[2].SSID != "GuestNetwork" || nets[2].SignalPercent != 40 || nets[2].Security != "open" {
		t.Errorf("unexpected third network: %+v", nets[2])
	}
}

func TestScanNoNmcli(t *testing.T) {
	nmcliPath = "non-existent-nmcli-bin"
	nets, err := Scan(context.Background())
	if err != nil {
		t.Fatalf("expected nil err without nmcli, got %v", err)
	}
	if nets != nil {
		t.Fatalf("expected nil networks without nmcli, got %+v", nets)
	}
}

func TestSupported(t *testing.T) {
	origGlobPhy := sysNetPhyGlob
	origGlobWire := sysNetWirelessGlob
	origDirIeee := sysIeee80211Dir
	origNmcli := nmcliPath
	defer func() {
		sysNetPhyGlob = origGlobPhy
		sysNetWirelessGlob = origGlobWire
		sysIeee80211Dir = origDirIeee
		nmcliPath = origNmcli
	}()

	tempDir := t.TempDir()
	sysNetPhyGlob = filepath.Join(tempDir, "net", "*", "phy80211")
	sysNetWirelessGlob = filepath.Join(tempDir, "net", "*", "wireless")
	sysIeee80211Dir = filepath.Join(tempDir, "ieee80211")
	nmcliPath = "no-such-nmcli-binary"

	// Should be false when no sysfs entries and no nmcli
	if Supported(context.Background()) {
		t.Error("expected Supported=false when no wifi hardware present")
	}

	// Create fake wireless sysfs entry
	fakeWifiDir := filepath.Join(tempDir, "net", "wlan0", "wireless")
	if err := os.MkdirAll(fakeWifiDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if !Supported(context.Background()) {
		t.Error("expected Supported=true when wireless sysfs entry is present")
	}
}

func TestParseActiveSSID(t *testing.T) {
	// Terse with yes
	out1 := "no:GuestNet\nyes:OfficeNet\nno:OtherNet\n"
	if got := parseActiveSSID(out1); got != "OfficeNet" {
		t.Errorf("parseActiveSSID got %q, want %q", got, "OfficeNet")
	}

	// Terse with *
	out2 := " :GuestNet\n*:HomeNet\n :OtherNet\n"
	if got := parseActiveSSID(out2); got != "HomeNet" {
		t.Errorf("parseActiveSSID got %q, want %q", got, "HomeNet")
	}

	// Terse with escaped colons
	out3 := "yes:Office\\:WiFi\n"
	if got := parseActiveSSID(out3); got != "Office:WiFi" {
		t.Errorf("parseActiveSSID got %q, want %q", got, "Office:WiFi")
	}

	// No active
	out4 := "no:GuestNet\nno:OfficeNet\n"
	if got := parseActiveSSID(out4); got != "" {
		t.Errorf("parseActiveSSID got %q, want empty", got)
	}
}

func TestDetectConnections(t *testing.T) {
	origNetDir := sysNetDir
	origNmcli := nmcliPath
	origIPChecker := ipChecker
	defer func() {
		sysNetDir = origNetDir
		nmcliPath = origNmcli
		ipChecker = origIPChecker
	}()

	tempDir := t.TempDir()
	sysNetDir = tempDir
	nmcliPath = "no-such-nmcli-binary"
	ipChecker = func(name string) bool { return true }

	// Create an ethernet interface with carrier=0 (unplugged cable) -> should NOT be connected
	enoDir := filepath.Join(tempDir, "eno1")
	if err := os.MkdirAll(enoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(enoDir, "carrier"), []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create nodexa0 bridge -> should be ignored
	nodexaDir := filepath.Join(tempDir, "nodexa0")
	if err := os.MkdirAll(nodexaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nodexaDir, "carrier"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Neither is connected yet
	conns0, _ := DetectConnections(context.Background())
	if len(conns0) != 0 {
		t.Errorf("expected empty conns, got %+v", conns0)
	}

	// Create an ethernet interface that IS plugged in (carrier=1)
	ethDir := filepath.Join(tempDir, "eth0")
	if err := os.MkdirAll(ethDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ethDir, "carrier"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	conns, ssid := DetectConnections(context.Background())
	if len(conns) != 1 || conns[0] != "ethernet" {
		t.Errorf("expected [ethernet], got %+v", conns)
	}
	if ssid != "" {
		t.Errorf("expected empty ssid, got %q", ssid)
	}

	// Add a wireless interface that is UP
	wifiDir := filepath.Join(tempDir, "wlan0")
	if err := os.MkdirAll(filepath.Join(wifiDir, "wireless"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wifiDir, "carrier"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	conns2, _ := DetectConnections(context.Background())
	if len(conns2) != 2 || conns2[0] != "ethernet" || conns2[1] != "wifi" {
		t.Errorf("expected [ethernet wifi], got %+v", conns2)
	}
}

func TestChangeFallback(t *testing.T) {
	origNmcli := nmcliPath
	origDefaultDir := DefaultConnectionDir
	origPersistentDir := PersistentConnectionDir
	defer func() {
		nmcliPath = origNmcli
		DefaultConnectionDir = origDefaultDir
		PersistentConnectionDir = origPersistentDir
		ClearLastError()
	}()

	tempDir := t.TempDir()
	runDir := filepath.Join(tempDir, "run")
	dataDir := filepath.Join(tempDir, "data")
	DefaultConnectionDir = runDir
	PersistentConnectionDir = dataDir

	// Setup initial working network "OldNetwork"
	oldCreds := Credentials{SSID: "OldNetwork", Password: "oldpassword123"}
	_, err := Apply(context.Background(), runDir, oldCreds)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Apply(context.Background(), dataDir, oldCreds)
	if err != nil {
		t.Fatal(err)
	}

	// Make nmcli fail to simulate connection failure to new network
	failScript := filepath.Join(tempDir, "fake-nmcli-fail")
	if err := os.WriteFile(failScript, []byte("#!/bin/sh\necho 'Error: connection activation failed' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	nmcliPath = failScript

	// Try changing to "BadNetwork"
	badCreds := Credentials{SSID: "BadNetwork", Password: "badpassword123"}
	changeErr := Change(context.Background(), badCreds)
	if changeErr == nil {
		t.Fatal("expected Change to fail, got nil")
	}

	// Verify LastError was populated
	lastErr := LastError()
	if !strings.Contains(lastErr, "BadNetwork") || !strings.Contains(lastErr, "OldNetwork") {
		t.Errorf("expected LastError to mention BadNetwork and OldNetwork, got: %q", lastErr)
	}

	// Verify keyfile in DefaultConnectionDir was reverted back to OldNetwork
	content, err := os.ReadFile(filepath.Join(runDir, "nodexa-wifi.nmconnection"))
	if err != nil {
		t.Fatalf("could not read reverted keyfile: %v", err)
	}
	if parseSSIDFromKeyfile(content) != "OldNetwork" {
		t.Errorf("keyfile was not reverted to OldNetwork, got ssid: %q", parseSSIDFromKeyfile(content))
	}
}

func TestChangeNoInternetFallback(t *testing.T) {
	origNmcli := nmcliPath
	origDefaultDir := DefaultConnectionDir
	origPersistentDir := PersistentConnectionDir
	origCheckInternet := CheckInternet
	defer func() {
		nmcliPath = origNmcli
		DefaultConnectionDir = origDefaultDir
		PersistentConnectionDir = origPersistentDir
		CheckInternet = origCheckInternet
		ClearLastError()
	}()

	tempDir := t.TempDir()
	runDir := filepath.Join(tempDir, "run")
	dataDir := filepath.Join(tempDir, "data")
	DefaultConnectionDir = runDir
	PersistentConnectionDir = dataDir

	// Setup initial working network "HomeWiFi"
	oldCreds := Credentials{SSID: "HomeWiFi", Password: "homepassword123"}
	_, err := Apply(context.Background(), runDir, oldCreds)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Apply(context.Background(), dataDir, oldCreds)
	if err != nil {
		t.Fatal(err)
	}

	// Fake nmcli that succeeds when activating connection
	succScript := filepath.Join(tempDir, "fake-nmcli-succ")
	if err := os.WriteFile(succScript, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	nmcliPath = succScript

	// Simulate CheckInternet failing (e.g. connected to AP, but no internet / WAN access)
	CheckInternet = func(ctx context.Context, iface string) error {
		return errors.New("cannot reach internet endpoints")
	}

	// Try changing to "CaptiveOrOfflineAP"
	newCreds := Credentials{SSID: "CaptiveOrOfflineAP", Password: "pass12345678"}
	changeErr := Change(context.Background(), newCreds)
	if changeErr == nil {
		t.Fatal("expected Change to fail due to no internet, got nil")
	}

	// Verify fallback reason in LastError
	lastErr := LastError()
	expectedErr := `No internet connection on "CaptiveOrOfflineAP"; reverted to "HomeWiFi"`
	if lastErr != expectedErr {
		t.Errorf("LastError got %q, want %q", lastErr, expectedErr)
	}
	if changeErr.Error() != expectedErr {
		t.Errorf("changeErr got %q, want %q", changeErr.Error(), expectedErr)
	}

	// Verify keyfile in DefaultConnectionDir was reverted back to HomeWiFi
	content, err := os.ReadFile(filepath.Join(runDir, "nodexa-wifi.nmconnection"))
	if err != nil {
		t.Fatalf("could not read reverted keyfile: %v", err)
	}
	if parseSSIDFromKeyfile(content) != "HomeWiFi" {
		t.Errorf("keyfile was not reverted to HomeWiFi, got ssid: %q", parseSSIDFromKeyfile(content))
	}
}

func TestChangeNoInternetNoOldNetwork(t *testing.T) {
	origNmcli := nmcliPath
	origDefaultDir := DefaultConnectionDir
	origPersistentDir := PersistentConnectionDir
	origCheckInternet := CheckInternet
	defer func() {
		nmcliPath = origNmcli
		DefaultConnectionDir = origDefaultDir
		PersistentConnectionDir = origPersistentDir
		CheckInternet = origCheckInternet
		ClearLastError()
	}()

	tempDir := t.TempDir()
	runDir := filepath.Join(tempDir, "run")
	dataDir := filepath.Join(tempDir, "data")
	DefaultConnectionDir = runDir
	PersistentConnectionDir = dataDir

	// No previous network setup!

	// Fake nmcli that succeeds in activation
	succScript := filepath.Join(tempDir, "fake-nmcli-succ")
	if err := os.WriteFile(succScript, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	nmcliPath = succScript

	// Simulate CheckInternet failing
	CheckInternet = func(ctx context.Context, iface string) error {
		return errors.New("timeout dialing 1.1.1.1")
	}

	newCreds := Credentials{SSID: "FirstOfflineNet", Password: "pass12345678"}
	changeErr := Change(context.Background(), newCreds)
	if changeErr == nil {
		t.Fatal("expected Change to fail, got nil")
	}

	lastErr := LastError()
	expectedErr := `No internet connection on "FirstOfflineNet"`
	if lastErr != expectedErr {
		t.Errorf("LastError got %q, want %q", lastErr, expectedErr)
	}

	// Verify keyfiles are removed
	if _, err := os.ReadFile(filepath.Join(runDir, "nodexa-wifi.nmconnection")); !os.IsNotExist(err) {
		t.Errorf("expected runtime keyfile to be removed, got err: %v", err)
	}
	if _, err := os.ReadFile(filepath.Join(dataDir, "nodexa-wifi.nmconnection")); !os.IsNotExist(err) {
		t.Errorf("expected persistent keyfile to be removed, got err: %v", err)
	}
}






