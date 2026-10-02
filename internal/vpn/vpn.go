// Package vpn defines the secure networking control surface nodexa-agent
// uses to join a device onto a VPN overlay, and (today) one real
// implementation of it: TailscaleProvider, used for ops-only direct SSH
// access by the support team. This is deliberately not the product's
// user-facing remote-access path -- that stays nodexa-backend's brokered
// SSH bridge (see plan.md §2) -- so it's scoped narrowly: join the tailnet
// if provisioned with a key, nothing more. A future Nodexa Connect product
// can still reuse this same Provider interface for a different backend.
package vpn

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Provider is the secure networking control surface exposed to
// nodexa-agent.
type Provider interface {
	// Connect establishes the secure network overlay.
	Connect(ctx context.Context) error
	// Disconnect tears it down.
	Disconnect(ctx context.Context) error
	// Status reports current connection state.
	Status() Status
	// IP returns this device's overlay-network IPv4 address. Only
	// meaningful after a successful Connect.
	IP(ctx context.Context) (string, error)
}

// Status describes VPN connectivity.
type Status struct {
	Connected bool   `json:"connected"`
	Endpoint  string `json:"endpoint,omitempty"`
}

// Unimplemented is a no-op Provider used wherever no VPN is configured.
type Unimplemented struct{}

func (Unimplemented) Connect(ctx context.Context) error      { return nil }
func (Unimplemented) Disconnect(ctx context.Context) error   { return nil }
func (Unimplemented) Status() Status                         { return Status{Connected: false} }
func (Unimplemented) IP(ctx context.Context) (string, error) { return "", nil }

// tailscaleBinPath is where the tailscale recipe (meta-tailscale, see
// kas/base.yml) installs the CLI. tailscaled itself is a separate,
// systemd-managed daemon (auto-started by its own unit regardless of
// whether AuthKey is set below -- it just sits idle, unauthenticated,
// until Connect calls "tailscale up"); this package only ever drives it
// through the CLI, never touches the daemon directly.
func tailscaleBinPath() string {
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p
	}
	return "/usr/bin/tailscale"
}

// TailscaleProvider joins the device onto a tailnet using a pre-provisioned
// reusable auth key (see internal/provisioning.Data.TailscaleAuthKey) --
// there is no per-device dynamic key issuance; the same key is baked into
// every image from a given build, same as CloudURL.
type TailscaleProvider struct {
	// AuthKey authenticates this device to the tailnet.
	AuthKey string
	// Hostname is how this device identifies itself in the tailnet --
	// callers should pass the device's stable Nodexa device ID, not the
	// kernel hostname, so it's recognizable in the Tailscale admin console.
	Hostname string
}

// Connect runs "tailscale up", authenticating with AuthKey and enabling
// Tailscale SSH (--ssh) so the support team can reach the device without a
// separate SSH server/credential of its own. Idempotent: safe to call on
// every boot regardless of whether the device is already joined.
func (p TailscaleProvider) Connect(ctx context.Context) error {
	var stderr bytes.Buffer
	bin := tailscaleBinPath()
	cmd := exec.CommandContext(ctx, bin, "up",
		"--authkey="+p.AuthKey,
		"--hostname="+p.Hostname,
		"--ssh",
		"--accept-dns=false", // this device's own resolver config, not the tailnet's, stays authoritative
	)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("vpn: tailscale up: %w: %s", err, stderr.String())
	}

	// "up --ssh" already turns Tailscale SSH on for this call, but that
	// flag only takes effect as part of an "up" invocation -- an explicit
	// "tailscale set --ssh" makes it a durable node setting instead, so it
	// stays on even across a future "tailscale up" run (manual re-auth,
	// key rotation, etc.) that forgets to repeat --ssh.
	stderr.Reset()
	setCmd := exec.CommandContext(ctx, bin, "set", "--ssh")
	setCmd.Stderr = &stderr
	if err := setCmd.Run(); err != nil {
		return fmt.Errorf("vpn: tailscale set --ssh: %w: %s", err, stderr.String())
	}
	return nil
}

// IP returns this device's tailnet IPv4 address via "tailscale ip -4", for
// nodexa-backend to expose to the support team as an ops-only direct SSH
// target (see connectVPN in cmd/nodexa-agent). Only meaningful once Connect
// has already succeeded.
func (p TailscaleProvider) IP(ctx context.Context) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, tailscaleBinPath(), "ip", "-4")
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("vpn: tailscale ip -4: %w: %s", err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Disconnect runs "tailscale down", taking the device off the tailnet
// without forgetting its identity (unlike "tailscale logout").
func (p TailscaleProvider) Disconnect(ctx context.Context) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, tailscaleBinPath(), "down")
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("vpn: tailscale down: %w: %s", err, stderr.String())
	}
	return nil
}

// Status reports whether the device currently has an active tailnet
// connection, via "tailscale status"'s exit code (0 = running and
// authenticated; nonzero otherwise, e.g. "Logged out" or the daemon being
// unreachable) rather than parsing its output, since this package only
// needs a connected/not-connected verdict, not full peer state.
func (p TailscaleProvider) Status() Status {
	err := exec.Command(tailscaleBinPath(), "status").Run()
	return Status{Connected: err == nil}
}
