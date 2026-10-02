// Package provisioning reads the optional config.json seeded onto the
// read-only nodexa-provisioning partition at image-flash time. It is the
// device's side of a one-way channel: a future Nodexa Cloud UI writes WiFi
// credentials, a cloud/backend URL override, and a fleet ID into a
// downloaded image before it's flashed; nodexa-agent reads that file back
// on every boot.
//
// This is deliberately not part of internal/config (the trusted,
// operator-owned /etc/nodexa store) or internal/transport (the future
// mTLS/signed-command Nodexa Cloud channel). It is a much smaller thing:
// a flash-time seed for bootstrapping a device into a fleet and pointing it
// at a backend, read fresh every boot from a partition nothing on-device
// can ever write back to (see layers/meta-nodexa/wic/nodexa-image.wks.in
// and the matching read-only fstab entry). If it's absent, the device is
// fully standalone -- consistent with edge devices legitimately operating
// offline.
package provisioning

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultMountPath is where the nodexa-provisioning partition is mounted.
const DefaultMountPath = "/mnt/nodexa-provisioning"

// configFileName is the file the future Nodexa Cloud UI writes into the
// partition before an image is downloaded/flashed.
const configFileName = "config.json"

// WifiCredentials is the network the device joins, applied by nodexa-agent
// through NetworkManager (internal/wifi). An empty Password is an open
// network.
type WifiCredentials struct {
	SSID     string `json:"ssid"`
	Password string `json:"password"`
}

// Location is where the device is installed, in decimal degrees.
type Location struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

// Data is the parsed contents of config.json. Every field is optional --
// an image can be flashed with none, some, or all of them set.
type Data struct {
	// CloudURL, if set, is the nodexa-backend base URL nodexa-agent
	// registers against on boot (see internal/backend).
	CloudURL *string `json:"cloud_url,omitempty"`
	// FleetID, if set, is sent along with registration so a future
	// "Nodexa Deploy" CLI can target container deployments per fleet.
	FleetID *string `json:"fleet_id,omitempty"`
	// Wifi, if set, is the Wi-Fi network to join; see WifiCredentials.
	Wifi *WifiCredentials `json:"wifi,omitempty"`
	// TailscaleAuthKey, if set, is a reusable Tailscale auth key nodexa-agent
	// uses to bring the device onto the tailnet (see internal/vpn) for
	// ops-only direct SSH access -- not the product's user-facing
	// remote-access path, which stays nodexa-backend's brokered SSH bridge.
	// A device with no key provisioned never joins any tailnet at all, same
	// standalone-by-default philosophy as CloudURL.
	TailscaleAuthKey *string `json:"tailscale_authkey,omitempty"`
	// Location, if set, is reported in every heartbeat. Unset,
	// nodexa-backend places the device by the public IP it heartbeats from.
	Location *Location `json:"location,omitempty"`
}

// Load reads config.json from mountPath. A missing file is not an error --
// same philosophy as internal/config.Load(): absence just means "use
// defaults" (here, "standalone device, no seed data"). A present-but-
// malformed file, or one carrying unrecognized fields, is a real error:
// this is flash-time input from build/backend tooling, not free-form user
// config, so silently ignoring typos would hide a broken provisioning
// pipeline rather than surface it.
func Load(mountPath string) (*Data, error) {
	primaryPath := filepath.Join(mountPath, configFileName)
	paths := []string{primaryPath}

	if mountPath == DefaultMountPath || mountPath == "" {
		paths = append(paths,
			"/etc/nodexa/config.json",
			"/var/lib/nodexa/config.json",
		)
		if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
			paths = append(paths, filepath.Join(localAppData, "nodex-supervisor", configFileName))
		}
		if progData := os.Getenv("ProgramData"); progData != "" {
			paths = append(paths, filepath.Join(progData, "nodexa", configFileName))
		}
		if exe, err := os.Executable(); err == nil {
			paths = append(paths, filepath.Join(filepath.Dir(exe), configFileName))
		}
		paths = append(paths, configFileName)
	}

	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("provisioning: %s: %w", path, err)
		}
		defer f.Close()

		var data Data
		dec := json.NewDecoder(f)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&data); err != nil {
			return nil, fmt.Errorf("provisioning: %s: invalid config.json: %w", path, err)
		}
		if l := data.Location; l != nil && (l.Latitude < -90 || l.Latitude > 90 || l.Longitude < -180 || l.Longitude > 180) {
			return nil, fmt.Errorf("provisioning: %s: location %v,%v out of range", path, l.Latitude, l.Longitude)
		}

		return &data, nil
	}

	return &Data{}, nil
}
