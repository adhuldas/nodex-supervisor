package identity

import (
	"runtime"
	"testing"
)

func TestParseDarwinIOReg(t *testing.T) {
	fixture := `+-o J815AP  <class IOPlatformExpertDevice, id 0x1000002ba, registered, matched>
    {
      "IOPlatformSerialNumber" = "H3FVHR974K"
      "model" = <"Mac17,4">
      "IOPlatformUUID" = "D4A0B4EB-3945-5A5D-9D6B-BDEC63B2B314"
    }`
	info := HardwareInfo{}
	parseDarwinIOReg(fixture, info)

	if got := info["platform_uuid"]; got != "D4A0B4EB-3945-5A5D-9D6B-BDEC63B2B314" {
		t.Fatalf("unexpected platform_uuid: %q", got)
	}
	if got := info["platform_serial"]; got != "H3FVHR974K" {
		t.Fatalf("unexpected platform_serial: %q", got)
	}
}

func TestParseDarwinNetworkSetup(t *testing.T) {
	fixture := `
Hardware Port: Ethernet Adapter (en2)
Device: en2
Ethernet Address: fa:fb:34:72:31:57

Hardware Port: Wi-Fi
Device: en0
Ethernet Address: fc:b2:14:6a:3a:9b

Hardware Port: Thunderbolt 1
Device: en1
Ethernet Address: 36:fd:5f:8d:f3:c0
`
	mac := parseDarwinNetworkSetup(fixture)
	if mac != "fc:b2:14:6a:3a:9b" {
		t.Fatalf("expected en0 MAC fc:b2:14:6a:3a:9b, got %q", mac)
	}
}

func TestThirdPartyProviderCollect(t *testing.T) {
	p := &ThirdPartyProvider{}
	info, err := p.Collect()
	if err != nil {
		t.Fatalf("Collect failed: %v", err)
	}
	if len(info) == 0 {
		t.Fatalf("Collect returned empty info on %s", runtime.GOOS)
	}

	// Ensure Manager can derive an identity without error
	mgr := NewManager(t.TempDir(), p)
	id, err := mgr.Load()
	if err != nil {
		t.Fatalf("Load with ThirdPartyProvider failed: %v", err)
	}
	if id.DeviceID == "" || id.Fingerprint == "" {
		t.Fatalf("incomplete identity derived: %+v", id)
	}
	if id.Provider != "third_party" {
		t.Fatalf("expected provider third_party, got %q", id.Provider)
	}
}
