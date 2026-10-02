package provisioning

import (
	"os"
	"path/filepath"
	"testing"
)

func writeConfig(t *testing.T, dir, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, configFileName), []byte(contents), 0o644); err != nil {
		t.Fatalf("writeConfig: %v", err)
	}
}

func TestLoadMissingFileReturnsEmptyData(t *testing.T) {
	dir := t.TempDir()

	data, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if data.CloudURL != nil || data.FleetID != nil || data.Wifi != nil || data.TailscaleAuthKey != nil {
		t.Fatalf("expected empty Data for missing file, got %+v", data)
	}
}

func TestLoadFullConfig(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{
		"cloud_url": "https://nodexa.elzora.tech/backend",
		"fleet_id": "fleet-west-1",
		"wifi": {"ssid": "test-network", "password": "hunter2"},
		"tailscale_authkey": "tskey-auth-example"
	}`)

	data, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if data.CloudURL == nil || *data.CloudURL != "https://nodexa.elzora.tech/backend" {
		t.Fatalf("unexpected CloudURL: %+v", data.CloudURL)
	}
	if data.FleetID == nil || *data.FleetID != "fleet-west-1" {
		t.Fatalf("unexpected FleetID: %+v", data.FleetID)
	}
	if data.Wifi == nil || data.Wifi.SSID != "test-network" || data.Wifi.Password != "hunter2" {
		t.Fatalf("unexpected Wifi: %+v", data.Wifi)
	}
	if data.TailscaleAuthKey == nil || *data.TailscaleAuthKey != "tskey-auth-example" {
		t.Fatalf("unexpected TailscaleAuthKey: %+v", data.TailscaleAuthKey)
	}
}

func TestLoadPartialConfig(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"fleet_id": "fleet-only"}`)

	data, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if data.FleetID == nil || *data.FleetID != "fleet-only" {
		t.Fatalf("unexpected FleetID: %+v", data.FleetID)
	}
	if data.CloudURL != nil {
		t.Fatalf("expected nil CloudURL, got %+v", data.CloudURL)
	}
	if data.Wifi != nil {
		t.Fatalf("expected nil Wifi, got %+v", data.Wifi)
	}
	if data.TailscaleAuthKey != nil {
		t.Fatalf("expected nil TailscaleAuthKey, got %+v", data.TailscaleAuthKey)
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"fleet_id": "fleet-a", "unexpected_field": "nope"}`)

	if _, err := Load(dir); err == nil {
		t.Fatal("expected error for unknown field")
	}
}

func TestLoadRejectsMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{not valid json`)

	if _, err := Load(dir); err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestLoadLocation(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"location": {"latitude": 9.93, "longitude": 76.26}}`)

	data, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if data.Location == nil || data.Location.Latitude != 9.93 || data.Location.Longitude != 76.26 {
		t.Fatalf("location = %+v", data.Location)
	}
}

func TestLoadRejectsOutOfRangeLocation(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, `{"location": {"latitude": 91, "longitude": 0}}`)

	if _, err := Load(dir); err == nil {
		t.Fatal("expected an error for latitude 91")
	}
}
