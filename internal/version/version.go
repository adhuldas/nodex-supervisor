// Package version carries build-time version metadata and embedded defaults for nodexa-agent.
//
// Values are populated via -ldflags at build time.
package version

import (
	"encoding/base64"
	"strings"
)

// These variables are overridden at link time with -ldflags:
var (
	// OSVersion is the Nodexa OS release version.
	OSVersion = "0.3.7"
	// AgentVersion is the nodexa-agent component version.
	AgentVersion = "0.3.7"
	// Commit is the git commit SHA the binary was built from.
	Commit = "unknown"
	// BuildDate is the UTC build timestamp in RFC3339 form.
	BuildDate = "unknown"

	// DefaultCloudURL is the optional backend URL embedded at release time.
	DefaultCloudURL = ""
	// DefaultTailscaleAuthKey is the optional Tailscale auth key embedded at release time.
	DefaultTailscaleAuthKey = ""
)

const secretMask byte = 0x5a

// EncodeSecret obfuscates a string so it is not stored in plaintext within the binary.
func EncodeSecret(val string) string {
	val = strings.TrimSpace(val)
	if val == "" {
		return ""
	}
	raw := []byte(val)
	for i := range raw {
		raw[i] ^= secretMask
	}
	return "enc:" + base64.StdEncoding.EncodeToString(raw)
}

// DecodeSecret decodes an obfuscated secret (prefixed with "enc:") or returns plain text.
func DecodeSecret(val string) string {
	val = strings.TrimSpace(val)
	if val == "" {
		return ""
	}
	if strings.HasPrefix(val, "enc:") {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(val, "enc:"))
		if err != nil {
			return ""
		}
		for i := range raw {
			raw[i] ^= secretMask
		}
		return string(raw)
	}
	return val
}

// GetDefaultCloudURL returns the decoded default Cloud URL, if configured.
func GetDefaultCloudURL() string {
	return DecodeSecret(DefaultCloudURL)
}

// GetDefaultTailscaleAuthKey returns the decoded default Tailscale Auth Key, if configured.
func GetDefaultTailscaleAuthKey() string {
	return DecodeSecret(DefaultTailscaleAuthKey)
}

// Info is a structured snapshot of version metadata, suitable for JSON
// serialization over the agent API.
type Info struct {
	OSVersion    string `json:"os_version"`
	AgentVersion string `json:"agent_version"`
	Commit       string `json:"build_commit"`
	BuildDate    string `json:"build_date"`
	Architecture string `json:"architecture"`
}
