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
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/gsm"
)

var (
	// DefaultConnectionDir is NetworkManager's runtime (in-memory) keyfile directory.
	DefaultConnectionDir = "/run/NetworkManager/system-connections"

	// PersistentConnectionDir is NetworkManager's persistent keyfile directory on nodexa-data.
	PersistentConnectionDir = "/var/lib/nodexa/network-connections"
)

// connectionName is the NetworkManager connection id, and keyfile name, of
// the provisioned network.
const connectionName = "nodexa-wifi"

// routeMetric is above systemd-networkd's DHCP default (1024), so a plugged
// in Ethernet cable stays the default route and Wi-Fi is the fallback.
const routeMetric = 2048

var (
	nmcliPath          = "nmcli"
	sysNetDir          = "/sys/class/net"
	sysNetPhyGlob      = "/sys/class/net/*/phy80211"
	sysNetWirelessGlob = "/sys/class/net/*/wireless"
	sysIeee80211Dir    = "/sys/class/ieee80211"
)

// Supported reports whether Wi-Fi hardware or interfaces are available on the host.
// It checks sysfs paths (/sys/class/net/*/phy80211, /sys/class/net/*/wireless, /sys/class/ieee80211)
// and falls back to inspecting `nmcli dev` if sysfs has no wireless devices.
func Supported(ctx context.Context) bool {
	if matches, err := filepath.Glob(sysNetPhyGlob); err == nil && len(matches) > 0 {
		return true
	}
	if matches, err := filepath.Glob(sysNetWirelessGlob); err == nil && len(matches) > 0 {
		return true
	}
	if entries, err := os.ReadDir(sysIeee80211Dir); err == nil && len(entries) > 0 {
		return true
	}
	if _, err := exec.LookPath(nmcliPath); err == nil {
		out, err := exec.CommandContext(ctx, nmcliPath, "-t", "-f", "TYPE", "device").Output()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if strings.TrimSpace(line) == "wifi" {
					return true
				}
			}
		}
	}
	return false
}

// WifiNetwork represents an available Wi-Fi network detected by scanning.
type WifiNetwork struct {
	SSID          string `json:"ssid"`
	SignalPercent int    `json:"signal_percent"`
	Security      string `json:"security"`
}

// Credentials is the network to join. An empty Password means an open
// network.
type Credentials struct {
	SSID     string `json:"ssid"`
	Password string `json:"password"`
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
	return KeyfileWithUUID(c, connectionUUID(connectionName))
}

// KeyfileWithUUID renders the NetworkManager keyfile for c with a specified UUID.
func KeyfileWithUUID(c Credentials, uuid string) string {
	if uuid == "" {
		uuid = connectionUUID(connectionName)
	}
	var b strings.Builder
	b.WriteString("[connection]\n")
	b.WriteString("id=" + connectionName + "\n")
	// Stable across boots, so NetworkManager treats the recreated keyfile as
	// the same connection.
	b.WriteString("uuid=" + uuid + "\n")
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

// connectionUUID is a name-based (version 5 style) UUID of a name.
func connectionUUID(name string) string {
	h := sha1.Sum([]byte(connectionName + "\x00" + name))
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
	return ApplyWithUUID(ctx, dir, c, connectionUUID(connectionName))
}

// ApplyWithUUID writes the keyfile for c with a specified UUID into dir and asks
// NetworkManager to reload it.
func ApplyWithUUID(ctx context.Context, dir string, c Credentials, uuid string) (changed bool, err error) {
	if err := c.Validate(); err != nil {
		return false, err
	}
	path := filepath.Join(dir, connectionName+".nmconnection")
	content := []byte(KeyfileWithUUID(c, uuid))
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

// Connect asks NetworkManager to activate the connection.
func Connect(ctx context.Context, id string) error {
	if _, err := exec.LookPath(nmcliPath); err != nil {
		return nil
	}
	if id == "" {
		id = connectionName
	}
	return exec.CommandContext(ctx, nmcliPath, "connection", "up", id).Run()
}

var (
	lastWifiError   string
	lastWifiErrorMu sync.Mutex
	activateTimeout = 15 * time.Second
)

// LastError returns the error from the most recent failed Wi-Fi connection attempt, if any.
func LastError() string {
	lastWifiErrorMu.Lock()
	defer lastWifiErrorMu.Unlock()
	return lastWifiError
}

// SetLastError records a Wi-Fi connection error.
func SetLastError(err string) {
	lastWifiErrorMu.Lock()
	lastWifiError = err
	lastWifiErrorMu.Unlock()
}

// ClearLastError clears any recorded Wi-Fi connection error.
func ClearLastError() {
	lastWifiErrorMu.Lock()
	lastWifiError = ""
	lastWifiErrorMu.Unlock()
}

// CheckInternet verifies internet connectivity through the specified network interface.
// Can be overridden in tests.
var CheckInternet = checkInternetConnectivity

// OnConnected is an optional hook invoked whenever a Wi-Fi connection is successfully
// established or changed.
var OnConnected func()

// Change updates the Wi-Fi credentials. It attempts to connect to the new network;
// if the connection fails or has no internet, it falls back to the previous connection
// (if one existed) and records the error so it can be reported to the cloud.
func Change(ctx context.Context, c Credentials) error {
	if err := c.Validate(); err != nil {
		return err
	}

	oldSSID := ConnectedSSID(ctx)
	oldPersistent, _ := os.ReadFile(filepath.Join(PersistentConnectionDir, connectionName+".nmconnection"))
	oldRuntime, _ := os.ReadFile(filepath.Join(DefaultConnectionDir, connectionName+".nmconnection"))
	if oldSSID == "" {
		if s := parseSSIDFromKeyfile(oldRuntime); s != "" {
			oldSSID = s
		} else if s := parseSSIDFromKeyfile(oldPersistent); s != "" {
			oldSSID = s
		}
	}

	// Preserve existing connection UUID if available, so NetworkManager treats this as an
	// in-place modification of the connection rather than creating duplicate profiles.
	uuid := parseUUIDFromKeyfile(oldPersistent)
	if uuid == "" {
		uuid = parseUUIDFromKeyfile(oldRuntime)
	}
	if uuid == "" {
		uuid = connectionUUID(connectionName)
	}

	// Disconnect active connection so NetworkManager starts fresh with the new profile
	if lookNmcli() {
		_ = exec.CommandContext(ctx, nmcliPath, "connection", "down", connectionName).Run()
	}

	// Apply new credentials to runtime keyfile directory
	if err := os.MkdirAll(DefaultConnectionDir, 0o700); err != nil {
		return fmt.Errorf("wifi: %w", err)
	}
	if _, err := ApplyWithUUID(ctx, DefaultConnectionDir, c, uuid); err != nil {
		return fmt.Errorf("wifi: %w", err)
	}

	// Also apply to persistent directory so NetworkManager's keyfile plugin (which is configured
	// with path=/var/lib/nodexa/network-connections) sees the new credentials and not the old ones.
	if len(oldPersistent) > 0 || dirExists(PersistentConnectionDir) {
		_ = os.MkdirAll(PersistentConnectionDir, 0o700)
		_, _ = ApplyWithUUID(ctx, PersistentConnectionDir, c, uuid)
	}
	reload(ctx)

	// Try activating the new connection
	connectErr := activateAndVerify(ctx, c.SSID)
	noInternet := false
	if connectErr == nil {
		// Connection succeeded; now verify internet connectivity
		wifiIface := ConnectedInterface(ctx)
		if internetErr := CheckInternet(ctx, wifiIface); internetErr != nil {
			connectErr = internetErr
			noInternet = true
		}
	}

	if connectErr == nil {
		// Succeeded with internet! Clear error and return
		ClearLastError()
		if OnConnected != nil {
			OnConnected()
		}
		return nil
	}

	// Connection failed or has no internet: fall back to previous credentials if available
	var revertMsg string
	if len(oldRuntime) > 0 || len(oldPersistent) > 0 {
		_ = restoreKeyfile(DefaultConnectionDir, oldRuntime)
		_ = restoreKeyfile(PersistentConnectionDir, oldPersistent)
		reload(ctx)
		if lookNmcli() {
			_ = exec.CommandContext(ctx, nmcliPath, "connection", "down", connectionName).Run()
		}
		_ = Connect(ctx, connectionName)
		if noInternet {
			revertMsg = fmt.Sprintf("No internet connection on %q; reverted to %q", c.SSID, oldSSID)
		} else {
			revertMsg = fmt.Sprintf("Failed to connect to %q: %v; reverted to %q", c.SSID, connectErr, oldSSID)
		}
	} else {
		_ = Remove(ctx, DefaultConnectionDir)
		_ = Remove(ctx, PersistentConnectionDir)
		reload(ctx)
		if noInternet {
			revertMsg = fmt.Sprintf("No internet connection on %q", c.SSID)
		} else {
			revertMsg = fmt.Sprintf("Failed to connect to %q: %v", c.SSID, connectErr)
		}
	}

	SetLastError(revertMsg)
	return errors.New(revertMsg)
}

func activateAndVerify(ctx context.Context, expectedSSID string) error {
	if _, err := exec.LookPath(nmcliPath); err != nil {
		return nil
	}

	cmdCtx, cancel := context.WithTimeout(ctx, activateTimeout)
	defer cancel()

	out, err := exec.CommandContext(cmdCtx, nmcliPath, "connection", "up", connectionName).CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return errors.New(msg)
	}

	// If ConnectedSSID is already expectedSSID, or is empty (e.g. test environment), return immediately.
	connected := ConnectedSSID(cmdCtx)
	if connected == expectedSSID || connected == "" {
		return nil
	}

	// If ConnectedSSID reports a different network (e.g. still lingering on old network),
	// wait up to 5s for NetworkManager to complete the switch.
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case <-cmdCtx.Done():
			return cmdCtx.Err()
		case <-time.After(300 * time.Millisecond):
		}
		connected = ConnectedSSID(cmdCtx)
		if connected == expectedSSID || connected == "" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("connected to unexpected network %q", connected)
		}
	}
}

func restoreKeyfile(dir string, content []byte) error {
	if len(content) == 0 {
		return Remove(context.Background(), dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, connectionName+".nmconnection")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, content, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func parseUUIDFromKeyfile(content []byte) string {
	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "uuid=") {
			return strings.TrimPrefix(line, "uuid=")
		}
	}
	return ""
}

func parseSSIDFromKeyfile(content []byte) string {
	lines := strings.Split(string(content), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ssid=") {
			val := strings.TrimPrefix(line, "ssid=")
			parts := strings.Split(strings.TrimSuffix(val, ";"), ";")
			var b []byte
			validBytes := true
			for _, p := range parts {
				if p == "" {
					continue
				}
				num, err := strconv.Atoi(p)
				if err != nil || num < 0 || num > 255 {
					validBytes = false
					break
				}
				b = append(b, byte(num))
			}
			if validBytes && len(b) > 0 {
				return string(b)
			}
			return val
		}
	}
	return ""
}

// Scan lists visible Wi-Fi access points using nmcli.
// If Wi-Fi is not supported, nmcli is not installed, or the scan fails, Scan returns nil, nil.
func Scan(ctx context.Context) ([]WifiNetwork, error) {
	if !Supported(ctx) {
		return nil, nil
	}
	if _, err := exec.LookPath(nmcliPath); err != nil {
		return nil, nil
	}
	out, err := exec.CommandContext(ctx, nmcliPath, "-t", "-f", "SSID,SIGNAL,SECURITY", "dev", "wifi", "list").Output()
	if err != nil {
		return nil, nil
	}
	return ParseScanOutput(string(out)), nil
}

// ParseScanOutput parses terse nmcli output into a deduplicated, sorted slice of WifiNetwork.
func ParseScanOutput(output string) []WifiNetwork {
	lines := strings.Split(output, "\n")
	best := make(map[string]WifiNetwork)

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := parseTerseLine(line)
		if len(fields) < 2 {
			continue
		}
		ssid := strings.TrimSpace(fields[0])
		if ssid == "" || ssid == "--" {
			continue
		}
		sig, _ := strconv.Atoi(strings.TrimSpace(fields[1]))
		if sig < 0 {
			sig = 0
		} else if sig > 100 {
			sig = 100
		}
		secRaw := ""
		if len(fields) >= 3 {
			secRaw = fields[2]
		}
		sec := normalizeSecurity(secRaw)

		if prev, ok := best[ssid]; !ok || sig > prev.SignalPercent {
			best[ssid] = WifiNetwork{
				SSID:          ssid,
				SignalPercent: sig,
				Security:      sec,
			}
		}
	}

	result := make([]WifiNetwork, 0, len(best))
	for _, net := range best {
		result = append(result, net)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].SignalPercent != result[j].SignalPercent {
			return result[i].SignalPercent > result[j].SignalPercent
		}
		return result[i].SSID < result[j].SSID
	})
	return result
}

func parseTerseLine(line string) []string {
	var fields []string
	var cur strings.Builder
	escaped := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if escaped {
			cur.WriteByte(ch)
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == ':' {
			fields = append(fields, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(ch)
	}
	fields = append(fields, cur.String())
	return fields
}

func normalizeSecurity(sec string) string {
	s := strings.TrimSpace(sec)
	if s == "" || s == "--" {
		return "open"
	}
	upper := strings.ToUpper(s)
	if strings.Contains(upper, "WPA3") {
		return "wpa3-psk"
	}
	if strings.Contains(upper, "WPA2") {
		return "wpa2-psk"
	}
	if strings.Contains(upper, "WPA") {
		return "wpa-psk"
	}
	if strings.Contains(upper, "WEP") {
		return "wep"
	}
	return strings.ToLower(s)
}

// ConnectedSSID returns the SSID of the active Wi-Fi connection, or "" if not connected.
func ConnectedSSID(ctx context.Context) string {
	if lookNmcli() {
		// Try ACTIVE,SSID
		if out, err := exec.CommandContext(ctx, nmcliPath, "-t", "-f", "ACTIVE,SSID", "dev", "wifi").Output(); err == nil {
			if ssid := parseActiveSSID(string(out)); ssid != "" {
				return ssid
			}
		}
		// Also try IN-USE,SSID
		if out, err := exec.CommandContext(ctx, nmcliPath, "-t", "-f", "IN-USE,SSID", "dev", "wifi", "list").Output(); err == nil {
			if ssid := parseActiveSSID(string(out)); ssid != "" {
				return ssid
			}
		}
	}
	if _, err := exec.LookPath("iwgetid"); err == nil {
		if out, err := exec.CommandContext(ctx, "iwgetid", "-r").Output(); err == nil {
			ssid := strings.TrimSpace(string(out))
			if ssid != "" {
				return ssid
			}
		}
	}
	return ""
}

// parseActiveSSID parses terse output where the first field indicates active status ("yes" or "*").
func parseActiveSSID(output string) string {
	for _, line := range strings.Split(output, "\n") {
		fields := parseTerseLine(strings.TrimSpace(line))
		if len(fields) >= 2 {
			active := strings.ToLower(strings.TrimSpace(fields[0]))
			if active == "yes" || active == "*" {
				ssid := strings.TrimSpace(fields[1])
				if ssid != "" && ssid != "--" {
					return ssid
				}
			}
		}
	}
	return ""
}

// DetectConnections checks the system for active network connections ("ethernet", "wifi")
// and returns the active connection types and the connected Wi-Fi SSID if Wi-Fi is connected.
func DetectConnections(ctx context.Context) ([]string, string) {
	hasEth := false
	hasWifi := false
	wifiSSID := ConnectedSSID(ctx)
	if wifiSSID != "" {
		hasWifi = true
	}

	entries, err := os.ReadDir(sysNetDir)
	if err == nil {
		for _, entry := range entries {
			name := entry.Name()
			ifaceDir := filepath.Join(sysNetDir, name)
			if isIgnoredInterface(ifaceDir, name) {
				continue
			}
			isWireless := isWirelessIface(ifaceDir)

			if isWireless {
				if !hasWifi && isWirelessActive(ifaceDir, name) {
					hasWifi = true
				}
			} else {
				if !hasEth && isEthernetActive(ifaceDir, name) {
					hasEth = true
				}
			}
		}
	}

	// Corroborate with nmcli dev if sysfs was inconclusive
	if (!hasEth || !hasWifi) && lookNmcli() {
		if out, err := exec.CommandContext(ctx, nmcliPath, "-t", "-f", "TYPE,STATE", "dev").Output(); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				fields := parseTerseLine(strings.TrimSpace(line))
				if len(fields) >= 2 && fields[1] == "connected" {
					if fields[0] == "ethernet" {
						hasEth = true
					} else if fields[0] == "wifi" {
						hasWifi = true
					}
				}
			}
		}
	}

	var conns []string
	if hasEth {
		conns = append(conns, "ethernet")
	}
	if hasWifi {
		conns = append(conns, "wifi")
	}
	if gsm.Connected(ctx) {
		conns = append(conns, "gsm")
	}

	return conns, wifiSSID
}

func lookNmcli() bool {
	_, err := exec.LookPath(nmcliPath)
	return err == nil
}

func isIgnoredInterface(ifaceDir string, name string) bool {
	if name == "lo" {
		return true
	}
	for _, prefix := range []string{
		"docker", "br-", "veth", "tailscale", "tun", "tap",
		"wg", "dummy", "nodexa", "virbr", "cni", "sit", "p2p-dev",
	} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	// If the "bridge" directory exists, it is a virtual bridge
	if _, err := os.Stat(filepath.Join(ifaceDir, "bridge")); err == nil {
		return true
	}
	return false
}

func isWirelessIface(ifaceDir string) bool {
	if _, err := os.Stat(filepath.Join(ifaceDir, "wireless")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(ifaceDir, "phy80211")); err == nil {
		return true
	}
	return false
}

var ipChecker = hasAssignedIP

func isEthernetActive(ifaceDir string, ifaceName string) bool {
	// Must have carrier == "1" (cable plugged in and link established).
	// If carrier is "0" or file cannot be read, cable is unplugged.
	b, err := os.ReadFile(filepath.Join(ifaceDir, "carrier"))
	if err != nil || strings.TrimSpace(string(b)) != "1" {
		return false
	}
	// Must also have an assigned, non-loopback, non-link-local IP address
	return ipChecker(ifaceName)
}

func isWirelessActive(ifaceDir string, ifaceName string) bool {
	b, err := os.ReadFile(filepath.Join(ifaceDir, "carrier"))
	if err == nil && strings.TrimSpace(string(b)) != "1" {
		return false
	}
	return ipChecker(ifaceName)
}

func hasAssignedIP(ifaceName string) bool {
	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return false
	}
	if iface.Flags&net.FlagUp == 0 {
		return false
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return false
	}
	for _, addr := range addrs {
		var ip net.IP
		switch v := addr.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() {
			return true
		}
	}
	return false
}

// ConnectedInterface returns the name of the Wi-Fi network interface currently connected
// to the provisioned connection, or any active wireless interface.
func ConnectedInterface(ctx context.Context) string {
	if lookNmcli() {
		// First try finding the device for connectionName
		out, err := exec.CommandContext(ctx, nmcliPath, "-t", "-f", "DEVICE,CONNECTION", "dev").Output()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				fields := parseTerseLine(strings.TrimSpace(line))
				if len(fields) >= 2 && fields[1] == connectionName {
					return fields[0]
				}
			}
		}
		// Next try any connected wifi device
		out, err = exec.CommandContext(ctx, nmcliPath, "-t", "-f", "DEVICE,TYPE,STATE", "dev").Output()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				fields := parseTerseLine(strings.TrimSpace(line))
				if len(fields) >= 3 && fields[1] == "wifi" && fields[2] == "connected" {
					return fields[0]
				}
			}
		}
	}

	// Fallback to sysNetDir
	entries, err := os.ReadDir(sysNetDir)
	if err == nil {
		for _, entry := range entries {
			name := entry.Name()
			ifaceDir := filepath.Join(sysNetDir, name)
			if isIgnoredInterface(ifaceDir, name) {
				continue
			}
			if isWirelessIface(ifaceDir) && isWirelessActive(ifaceDir, name) {
				return name
			}
		}
	}
	return ""
}

func checkInternetConnectivity(ctx context.Context, ifaceName string) error {
	// If interface is known, wait briefly for DHCP/IP if not yet assigned
	if ifaceName != "" && !hasAssignedIP(ifaceName) {
		deadline := time.Now().Add(4 * time.Second)
		for !hasAssignedIP(ifaceName) {
			if time.Now().After(deadline) {
				return errors.New("no IP address assigned to interface")
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(250 * time.Millisecond):
			}
		}
	}

	// If NetworkManager is available, check its connectivity assessment
	if lookNmcli() {
		cmdCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		out, err := exec.CommandContext(cmdCtx, nmcliPath, "networking", "connectivity", "check").Output()
		cancel()
		if err == nil {
			status := strings.TrimSpace(string(out))
			if status == "none" || status == "portal" || status == "limited" {
				return fmt.Errorf("NetworkManager reported %s connectivity", status)
			}
		}
	}

	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	dialer := &net.Dialer{
		Timeout: 3 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			var opErr error
			err := c.Control(func(fd uintptr) {
				opErr = bindToDevice(fd, ifaceName)
			})
			if err != nil {
				return err
			}
			return opErr
		},
	}

	transport := &http.Transport{
		DialContext:       dialer.DialContext,
		DisableKeepAlives: true,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   4 * time.Second,
	}

	// Probe 1: Google generate_204 endpoint
	req1, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "http://connectivitycheck.gstatic.com/generate_204", nil)
	if err == nil {
		resp, err := client.Do(req1)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusNoContent {
				return nil
			}
			if resp.StatusCode == http.StatusOK || (resp.StatusCode >= 300 && resp.StatusCode < 400) {
				return errors.New("captive portal detected, no direct internet access")
			}
		}
	}

	// Probe 2: Cloudflare generate_204 endpoint
	req2, err := http.NewRequestWithContext(probeCtx, http.MethodGet, "http://cp.cloudflare.com/generate_204", nil)
	if err == nil {
		resp, err := client.Do(req2)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusNoContent {
				return nil
			}
		}
	}

	// Probe 3: Direct TCP connection to public DNS 1.1.1.1:53 or 8.8.8.8:53
	conn, err := dialer.DialContext(probeCtx, "tcp", "1.1.1.1:53")
	if err == nil {
		_ = conn.Close()
		return nil
	}
	conn, err = dialer.DialContext(probeCtx, "tcp", "8.8.8.8:53")
	if err == nil {
		_ = conn.Close()
		return nil
	}

	return errors.New("cannot reach internet endpoints")
}



