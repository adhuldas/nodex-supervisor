package identity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// fakeProvider lets tests control exactly which hardware info is "collected".
type fakeProvider struct {
	name string
	info HardwareInfo
	err  error
}

func (f *fakeProvider) Name() string                   { return f.name }
func (f *fakeProvider) Collect() (HardwareInfo, error) { return f.info, f.err }

func TestFingerprintIsDeterministic(t *testing.T) {
	hw1 := HardwareInfo{"dmi_product_uuid": "ABC-123", "primary_nic_mac": "AA:BB:CC"}
	hw2 := HardwareInfo{"primary_nic_mac": "aa:bb:cc", "dmi_product_uuid": "abc-123"} // different order/case

	f1, _ := Fingerprint(hw1)
	f2, _ := Fingerprint(hw2)

	if f1 != f2 {
		t.Fatalf("expected identical fingerprints regardless of key order/case, got %s vs %s", f1, f2)
	}
}

func TestFingerprintChangesWithHardware(t *testing.T) {
	f1, _ := Fingerprint(HardwareInfo{"dmi_product_uuid": "AAAA"})
	f2, _ := Fingerprint(HardwareInfo{"dmi_product_uuid": "BBBB"})
	if f1 == f2 {
		t.Fatalf("expected different fingerprints for different hardware info")
	}
}

func TestDeviceIDFormat(t *testing.T) {
	fp, _ := Fingerprint(HardwareInfo{"dmi_product_uuid": "AAAA"})
	id := DeviceIDFromFingerprint(fp)
	if len(id) != 32 {
		t.Fatalf("unexpected device id length: %q", id)
	}
}

func TestDistinctDevicesGetDistinctIDs(t *testing.T) {
	seen := map[string]bool{}
	for _, hw := range []HardwareInfo{
		{"primary_nic_mac": "b0:d5:cc:fa:11:01", "cpu_serial": "1"},
		{"primary_nic_mac": "b0:d5:cc:fa:11:02", "cpu_serial": "1"},
		{"cpu_serial": "2"},
		{"cpu_serial": "3"},
	} {
		id, err := NewManager(t.TempDir(), &fakeProvider{name: "test", info: hw}).Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if id.DeviceID == collidingDeviceID || seen[id.DeviceID] {
			t.Fatalf("device id %s for %v is not unique", id.DeviceID, hw)
		}
		seen[id.DeviceID] = true
	}
}

func TestDeviceIDIsHashOfMAC(t *testing.T) {
	// The same rule as the ESP32 firmware: sha256 of the bare lower-case MAC.
	hw := HardwareInfo{"primary_nic_mac": "B0:D5:CC:FA:11:01", "cpu_serial": "1"}
	fp, _ := Fingerprint(hw)
	if got, want := DeviceIDFromFingerprint(deviceIDSource(hw, fp)), DeviceIDFromFingerprint("b0d5ccfa1101"); got != want {
		t.Fatalf("device id = %s, want %s", got, want)
	}

	// Locally administered and all-zero MACs aren't burned in: fall back
	// to the fingerprint.
	for _, mac := range []string{"02:11:22:33:44:55", "00:00:00:00:00:00", "garbage"} {
		hw := HardwareInfo{"primary_nic_mac": mac, "cpu_serial": "1"}
		fp, _ := Fingerprint(hw)
		if src := deviceIDSource(hw, fp); src != fp {
			t.Fatalf("MAC %s: device id derived from %q, want the fingerprint", mac, src)
		}
	}
}

func TestManagerReplacesCollidingPersistedID(t *testing.T) {
	dir := t.TempDir()
	provider := &fakeProvider{name: "test", info: HardwareInfo{"primary_nic_mac": "b0:d5:cc:fa:11:01"}}
	want, err := NewManager(dir, provider).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// What an older agent persisted.
	old := *want
	old.DeviceID = collidingDeviceID
	b, _ := json.Marshal(old)
	if err := os.WriteFile(filepath.Join(dir, identityFileName), b, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := NewManager(dir, provider).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.DeviceID != want.DeviceID {
		t.Fatalf("device id = %s, want %s", got.DeviceID, want.DeviceID)
	}
	// ...and the fixed ID is persisted.
	again, _ := NewManager(dir, &fakeProvider{name: "test", err: os.ErrNotExist}).Load()
	if again == nil || again.DeviceID != want.DeviceID {
		t.Fatalf("fixed device id was not persisted: %+v", again)
	}
}

func TestManagerLoadIsStableAcrossReboots(t *testing.T) {
	dir := t.TempDir()
	provider := &fakeProvider{name: "test", info: HardwareInfo{"dmi_product_uuid": "stable-uuid"}}
	mgr := NewManager(dir, provider)

	first, err := mgr.Load()
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}

	// Simulate a reboot: new Manager instance, same persistent directory.
	mgr2 := NewManager(dir, provider)
	second, err := mgr2.Load()
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}

	if first.DeviceID != second.DeviceID {
		t.Fatalf("device id changed across simulated reboot: %s vs %s", first.DeviceID, second.DeviceID)
	}
}

func TestManagerLoadIsStableAcrossReflashWhenHardwareUnchanged(t *testing.T) {
	provider := &fakeProvider{name: "test", info: HardwareInfo{"dmi_product_uuid": "stable-uuid"}}

	dir1 := t.TempDir()
	id1, err := NewManager(dir1, provider).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Simulate reflashing persistent storage too (fresh identity dir), but
	// on the SAME underlying hardware: the fingerprint must be re-derivable
	// to the same value.
	dir2 := t.TempDir()
	id2, err := NewManager(dir2, provider).Load()
	if err != nil {
		t.Fatalf("Load after simulated reflash: %v", err)
	}

	if id1.DeviceID != id2.DeviceID {
		t.Fatalf("device id not reproducible after simulated reflash: %s vs %s", id1.DeviceID, id2.DeviceID)
	}
}

func TestManagerPersistsToDisk(t *testing.T) {
	dir := t.TempDir()
	provider := &fakeProvider{name: "test", info: HardwareInfo{"dmi_product_uuid": "abc"}}
	if _, err := NewManager(dir, provider).Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, identityFileName)); err != nil {
		t.Fatalf("expected identity file to be persisted: %v", err)
	}
}

func TestManagerErrorsWithNoHardwareInfo(t *testing.T) {
	dir := t.TempDir()
	provider := &fakeProvider{name: "empty", info: HardwareInfo{}}
	if _, err := NewManager(dir, provider).Load(); err == nil {
		t.Fatalf("expected error when provider yields no hardware info")
	}
}
