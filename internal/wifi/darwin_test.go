package wifi

import "testing"

func TestDarwinParsers(t *testing.T) {
	ports := parseHardwarePorts("Hardware Port: Ethernet Adapter (en2)\nDevice: en2\nEthernet Address: fa:fb\n\nHardware Port: Thunderbolt Bridge\nDevice: bridge0\n\nHardware Port: Wi-Fi\nDevice: en0\nEthernet Address: fc:b2\n")
	if len(ports) != 3 || ports[2].Device != "en0" || !isWifiPort(ports[2]) || !isWiredPort(ports[0]) || isWiredPort(ports[1]) || isWifiPort(ports[1]) {
		t.Fatalf("ports = %+v", ports)
	}
	if !linkActive("en0: flags=8863<UP>\n\tinet 192.168.31.110 netmask 0xffffff00\n\tstatus: active\n") {
		t.Error("active link missed")
	}
	if linkActive("en0: flags=8863<UP>\n\tstatus: inactive\n") || linkActive("\tinet6 fe80::1\n\tstatus: active\n") {
		t.Error("inactive or IPv6-only link counted")
	}
	if got := parseAirportNetwork("Current Wi-Fi Network: Home Net\n"); got != "Home Net" {
		t.Errorf("airport network = %q", got)
	}
	if got := parseAirportNetwork("You are not associated with an AirPort network.\n"); got != "" {
		t.Errorf("not associated = %q", got)
	}
	if got := parseSummarySSID("  InterfaceType : WiFi\n  SSID : Office\n"); got != "Office" {
		t.Errorf("summary ssid = %q", got)
	}
	if got := parseSummarySSID("  SSID : <redacted>\n"); got != "" {
		t.Errorf("redacted ssid = %q", got)
	}
}
