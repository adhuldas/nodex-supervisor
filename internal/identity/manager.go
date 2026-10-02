package identity

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Identity is the persisted, deterministic local device identity.
//
// DeviceID is what the rest of Nodexa OS refers to as the
// "nodexa-device-id". It is derived from, but shorter than, Fingerprint.
// Fingerprint is retained in full so a future Nodexa Cloud registration flow
// can map it to a permanent Nodexa Device ID without re-deriving anything on
// the device.
type Identity struct {
	DeviceID    string    `json:"device_id"`
	Fingerprint string    `json:"hardware_fingerprint"`
	Provider    string    `json:"provider"`
	SourceKeys  []string  `json:"source_keys"`
	CreatedAt   time.Time `json:"created_at"`
}

// Manager derives and persists device identity.
type Manager struct {
	dir      string
	provider Provider
}

// NewManager creates an identity Manager that persists under dir and
// collects hardware information via provider.
func NewManager(dir string, provider Provider) *Manager {
	return &Manager{dir: dir, provider: provider}
}

const identityFileName = "identity.json"

// Load returns this device's identity, deriving and persisting it on first
// use. On every subsequent call (including across reboots, power cycles,
// and OS updates) it returns the previously persisted identity as long as
// the persistent data partition (/var/lib/nodexa) survives.
//
// If the persisted record is missing -- e.g. after a full reflash that also
// wiped persistent storage -- the identity is re-derived from hardware. As
// long as the underlying hardware (or, for QEMU, the pinned virtual machine
// UUID) is unchanged, this reproduces the exact same DeviceID, which is the
// entire point of fingerprinting real hardware properties instead of
// generating a random ID at first boot.
func (m *Manager) Load() (*Identity, error) {
	existing, err := m.readPersisted()
	if err == nil && existing.DeviceID != collidingDeviceID {
		return existing, nil
	}
	if err == nil {
		// Persisted by an agent that derived every device's ID from an
		// empty string (see deviceIDSource): re-derive, or all such
		// devices keep sharing one ID in Nodexa Cloud.
		log.Printf("identity: device ID %s is shared by all devices; deriving a unique one", existing.DeviceID)
	}

	id, err := m.derive()
	if err != nil {
		return nil, err
	}
	if err := m.persist(id); err != nil {
		// Identity is still valid even if we couldn't persist it (e.g.
		// read-only filesystem misconfiguration); surface it but don't
		// fail the caller, since a running agent still needs an identity.
		return id, fmt.Errorf("identity: derived but failed to persist: %w", err)
	}
	return id, nil
}

func (m *Manager) path() string {
	return filepath.Join(m.dir, identityFileName)
}

func (m *Manager) readPersisted() (*Identity, error) {
	b, err := os.ReadFile(m.path())
	if err != nil {
		return nil, err
	}
	var id Identity
	if err := json.Unmarshal(b, &id); err != nil {
		return nil, fmt.Errorf("identity: corrupt identity file: %w", err)
	}
	if id.DeviceID == "" || id.Fingerprint == "" {
		return nil, fmt.Errorf("identity: incomplete identity file")
	}
	return &id, nil
}

func (m *Manager) persist(id *Identity) error {
	if err := os.MkdirAll(m.dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return err
	}

	tmp := m.path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path())
}

func (m *Manager) derive() (*Identity, error) {
	hw, err := m.provider.Collect()
	if err != nil {
		return nil, fmt.Errorf("identity: %s provider: %w", m.provider.Name(), err)
	}
	if len(hw) == 0 {
		return nil, fmt.Errorf("identity: %s provider returned no usable hardware information", m.provider.Name())
	}

	fingerprint, keys := Fingerprint(hw)
	return &Identity{
		DeviceID:    DeviceIDFromFingerprint(deviceIDSource(hw, fingerprint)),
		Fingerprint: fingerprint,
		Provider:    m.provider.Name(),
		SourceKeys:  keys,
		CreatedAt:   time.Now().UTC(),
	}, nil
}

// collidingDeviceID is the ID agents up to 0.1.x gave every device: they
// hashed hw["mac"], a key no Provider sets, i.e. always "".
var collidingDeviceID = DeviceIDFromFingerprint("")

// deviceIDSource picks what the device ID is hashed from: the primary NIC's
// MAC, which is burned into the SoC/NIC and so survives reflashing and SD
// card swaps (and is the same rule the ESP32 firmware uses), or else the
// full fingerprint. All-zero and locally administered (randomly generated,
// e.g. USB gadget) MACs aren't stable, so they don't count.
func deviceIDSource(hw HardwareInfo, fingerprint string) string {
	mac := strings.ToLower(strings.TrimSpace(hw["primary_nic_mac"]))
	if hwMAC, err := net.ParseMAC(mac); err == nil && len(hwMAC) == 6 &&
		hwMAC[0]&0x02 == 0 && !bytes.Equal(hwMAC, make(net.HardwareAddr, 6)) {
		return mac
	}
	return fingerprint
}

// Fingerprint normalizes hardware info into a deterministic SHA-256 hash.
// Normalization: keys are sorted, values are trimmed and lower-cased, and
// the canonical "key=value\n" pairs are concatenated before hashing -- so
// the same underlying hardware always yields the same fingerprint
// regardless of collection order.
func Fingerprint(hw HardwareInfo) (fingerprint string, sourceKeys []string) {
	keys := make([]string, 0, len(hw))
	for k := range hw {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var b strings.Builder
	for _, k := range keys {
		v := strings.ToLower(strings.TrimSpace(hw[k]))
		if v == "" {
			continue
		}
		fmt.Fprintf(&b, "%s=%s\n", k, v)
	}

	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:]), keys
}

// DeviceIDFromFingerprint derives the 32-hex-character device ID from src
// (see deviceIDSource): the first 128 bits of sha256 of src, lower-cased
// and with MAC separators removed. The full fingerprint remains the
// canonical value used for any future backend identity mapping.
func DeviceIDFromFingerprint(src string) string {
	src = strings.ToLower(strings.TrimSpace(src))
	src = strings.ReplaceAll(src, ":", "")
	src = strings.ReplaceAll(src, "-", "")

	sum := sha256.Sum256([]byte(src))

	return hex.EncodeToString(sum[:16])
}
