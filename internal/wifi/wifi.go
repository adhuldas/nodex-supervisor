// Package wifi applies the Wi-Fi network seeded into config.json (see
// internal/provisioning) by handing it to NetworkManager, which owns every
// wireless interface on Nodexa OS (nodexa-system-config's
// 10-nodexa-nm-unmanaged.conf). systemd-networkd keeps wired Ethernet.
//
// The connection is written as a keyfile under /run, not /etc: the root
// filesystem is read-only, and config.json is re-read on every boot anyway,
// so the provisioned network is simply recreated each boot from its seed.
// Networks added by hand with nmcli are separate and persist on nodexa-data
// (NetworkManager's keyfile path, same conf file).
package wifi

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultConnectionDir is NetworkManager's runtime (in-memory) keyfile
// directory.
const DefaultConnectionDir = "/run/NetworkManager/system-connections"

// connectionName is the NetworkManager connection id, and keyfile name, of
// the provisioned network.
const connectionName = "nodexa-wifi"

// routeMetric is above systemd-networkd's DHCP default (1024), so a plugged
// in Ethernet cable stays the default route and Wi-Fi is the fallback.
const routeMetric = 2048

var nmcliPath = "nmcli"

// Credentials is the network to join. An empty Password means an open
// network.
type Credentials struct {
	SSID     string
	Password string
}

// Validate applies the same limits nodexa-user-service enforces when it
// writes config.json: SSID 1-32 bytes, WPA passphrase 8-63 characters or a
// 64-digit hex PSK, or empty for an open network.
func (c Credentials) Validate() error {
	if n := len(c.SSID); n < 1 || n > 32 {
		return fmt.Errorf("wifi: SSID must be 1-32 bytes, got %d", n)
	}
	if c.Password == "" {
		return nil
	}
	if strings.ContainsAny(c.Password, "\r\n\x00") {
		return errors.New("wifi: password contains a control character")
	}
	if n := len(c.Password); n < 8 || n > 64 {
		return fmt.Errorf("wifi: password must be 8-63 characters (or a 64-digit hex key), got %d", n)
	}
	return nil
}

// Keyfile renders the NetworkManager keyfile for c.
func Keyfile(c Credentials) string {
	var b strings.Builder
	b.WriteString("[connection]\n")
	b.WriteString("id=" + connectionName + "\n")
	// Stable across boots, so NetworkManager treats the recreated keyfile as
	// the same connection.
	b.WriteString("uuid=" + connectionUUID(c.SSID) + "\n")
	b.WriteString("type=wifi\n")
	b.WriteString("autoconnect=true\n")
	// Keep retrying forever: the access point may come up after the device.
	b.WriteString("autoconnect-retries=0\n")
	b.WriteString("\n[wifi]\n")
	b.WriteString("mode=infrastructure\n")
	// Written as a byte list, which the keyfile format accepts for any SSID,
	// so no SSID character needs escaping.
	b.WriteString("ssid=" + ssidBytes(c.SSID) + "\n")
	if c.Password != "" {
		b.WriteString("\n[wifi-security]\n")
		b.WriteString("key-mgmt=wpa-psk\n")
		b.WriteString("psk=" + escapeValue(c.Password) + "\n")
	}
	fmt.Fprintf(&b, "\n[ipv4]\nmethod=auto\nroute-metric=%d\n", routeMetric)
	fmt.Fprintf(&b, "\n[ipv6]\nmethod=auto\nroute-metric=%d\n", routeMetric)
	return b.String()
}

// connectionUUID is a name-based (version 5 style) UUID of the SSID.
func connectionUUID(ssid string) string {
	h := sha1.Sum([]byte(connectionName + "\x00" + ssid))
	h[6] = (h[6] & 0x0f) | 0x50
	h[8] = (h[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

func ssidBytes(ssid string) string {
	var b strings.Builder
	for i := 0; i < len(ssid); i++ {
		b.WriteString(strconv.Itoa(int(ssid[i])))
		b.WriteByte(';')
	}
	return b.String()
}

// escapeValue escapes a keyfile (GKeyFile) string value: backslashes, and
// spaces, which GKeyFile would otherwise trim from the ends.
func escapeValue(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, " ", `\s`)
}

// Apply writes the keyfile for c into dir (DefaultConnectionDir on a
// device) and asks NetworkManager to reload it. Unchanged credentials are
// left alone. A NetworkManager that isn't running yet (or isn't installed,
// e.g. the QEMU-only machines) is not an error: NetworkManager reads the
// directory itself when it starts.
func Apply(ctx context.Context, dir string, c Credentials) (changed bool, err error) {
	if err := c.Validate(); err != nil {
		return false, err
	}
	path := filepath.Join(dir, connectionName+".nmconnection")
	content := []byte(Keyfile(c))
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, content) {
		return false, nil
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, fmt.Errorf("wifi: %w", err)
	}
	// NetworkManager ignores keyfiles readable by anyone but root.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		return false, fmt.Errorf("wifi: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return false, fmt.Errorf("wifi: %w", err)
	}

	reload(ctx)
	return true, nil
}

// Remove deletes the provisioned connection, for a config.json that no
// longer carries Wi-Fi. Since the keyfile lives under /run this only
// matters within a boot.
func Remove(ctx context.Context, dir string) error {
	err := os.Remove(filepath.Join(dir, connectionName+".nmconnection"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("wifi: %w", err)
	}
	reload(ctx)
	return nil
}

// reload is best effort: see Apply.
func reload(ctx context.Context) {
	if _, err := exec.LookPath(nmcliPath); err != nil {
		return
	}
	_ = exec.CommandContext(ctx, nmcliPath, "connection", "reload").Run()
}
