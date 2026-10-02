// Package transport defines the (not-yet-implemented) interface nodexa-agent
// will use to talk to Nodexa Cloud: mutually authenticated, certificate
// pinned, carrying signed commands and deployments.
//
// Nothing in Phase 1 depends on this package. It exists purely so the rest
// of nodexa-agent (event forwarding, command handling) can be written
// against a stable interface now, and wired up to a real implementation
// later without touching core agent logic.
//
// Planned trust model (see docs/architecture.md and docs/security.md):
//   - Pinned backend identity (no arbitrary CA trust)
//   - Mutual TLS with per-device certificates
//   - Signed commands and signed deployment manifests
//   - Certificate rotation without re-establishing device trust
//   - The backend relationship itself is not mutable via normal
//     /etc/nodexa configuration -- it is intentionally out of reach of
//     ordinary application-level config to protect against a compromised
//     workload redirecting a device to a rogue backend.
package transport

import "context"

// Transport is the future channel between nodexa-agent and Nodexa Cloud.
type Transport interface {
	// Connect establishes (or re-establishes) the backend connection.
	Connect(ctx context.Context) error

	// Send delivers an outbound message (telemetry, event, status) to the
	// backend.
	Send(ctx context.Context, kind string, payload []byte) error

	// Commands returns a channel of inbound, signature-verified commands
	// from the backend.
	Commands() <-chan Command

	// Connected reports current connectivity state.
	Connected() bool
}

// Command is a signed instruction received from Nodexa Cloud.
type Command struct {
	ID      string
	Kind    string
	Payload []byte
}

// Unimplemented is a no-op Transport used until a real implementation
// exists. It always reports disconnected and never errors, so callers can
// wire it in today without special-casing "no backend configured".
type Unimplemented struct{}

func (Unimplemented) Connect(ctx context.Context) error { return nil }
func (Unimplemented) Send(ctx context.Context, kind string, payload []byte) error { return nil }
func (Unimplemented) Commands() <-chan Command           { return nil }
func (Unimplemented) Connected() bool                    { return false }
