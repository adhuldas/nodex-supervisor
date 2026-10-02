// Package events is Nodexa Agent's structured event bus. Every notable
// lifecycle transition (boot, identity ready, container started, ...) is
// emitted as a named NODEXA_* event: logged to journald for operators today,
// and shaped so it can be forwarded to a future Nodexa Cloud telemetry
// pipeline unchanged.
package events

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-systemd/v22/journal"
)

// Well-known Nodexa event identifiers. Keep this list in sync with
// docs/architecture.md.
const (
	Boot                    = "NODEXA_BOOT"
	AgentStarted            = "NODEXA_AGENT_STARTED"
	AgentReady              = "NODEXA_AGENT_READY"
	IdentityReady           = "NODEXA_IDENTITY_READY"
	NetworkReady            = "NODEXA_NETWORK_READY"
	NetworkConnected        = "NODEXA_NETWORK_CONNECTED"
	HealthCheck             = "NODEXA_HEALTH_CHECK"
	HealthReport            = "NODEXA_HEALTH_REPORT"
	ContainerRuntimeReady   = "NODEXA_CONTAINER_RUNTIME_READY"
	ContainerStarted        = "NODEXA_CONTAINER_STARTED"
	ContainerStopped        = "NODEXA_CONTAINER_STOPPED"
	ContainerRemoved        = "NODEXA_CONTAINER_REMOVED"
	ContainerFailed         = "NODEXA_CONTAINER_FAILED"
	UpdateReady             = "NODEXA_UPDATE_READY"
	DeviceRegistered        = "NODEXA_DEVICE_REGISTERED"
	DeviceRegisterFailed    = "NODEXA_DEVICE_REGISTER_FAILED"

	// Future events (Nodexa Deploy / Connect / Update). Reserved here so
	// downstream consumers (log parsers, future Cloud agent) can already
	// treat these identifiers as stable, even though nothing emits them yet.
	DeploymentStarted   = "NODEXA_DEPLOYMENT_STARTED"
	DeploymentProgress  = "NODEXA_DEPLOYMENT_PROGRESS"
	DeploymentCompleted = "NODEXA_DEPLOYMENT_COMPLETED"
	DeploymentFailed    = "NODEXA_DEPLOYMENT_FAILED"
	VPNConnected        = "NODEXA_VPN_CONNECTED"
	UpdateAvailable     = "NODEXA_UPDATE_AVAILABLE"
	UpdateCompleted     = "NODEXA_UPDATE_COMPLETED"
)

// Event is one structured, timestamped occurrence on the bus.
type Event struct {
	ID      string            `json:"id"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`
	Time    time.Time         `json:"time"`
}

// Bus records recent events in memory (for the local API/CLI) and forwards
// every event to journald (for operators, `journalctl -t nodexa-agent`, and
// eventually a future telemetry shipper).
type Bus struct {
	mu       sync.Mutex
	buf      []Event
	capacity int
	journal  bool // whether journald is reachable; false falls back to stdlib log
}

// NewBus creates an event bus retaining up to capacity recent events.
func NewBus(capacity int) *Bus {
	return &Bus{
		capacity: capacity,
		journal:  journal.Enabled(),
	}
}

// Emit records and logs a structured event.
func (b *Bus) Emit(id, message string, fields map[string]string) {
	ev := Event{ID: id, Message: message, Fields: fields, Time: time.Now().UTC()}

	b.mu.Lock()
	b.buf = append(b.buf, ev)
	if len(b.buf) > b.capacity {
		b.buf = b.buf[len(b.buf)-b.capacity:]
	}
	b.mu.Unlock()

	vars := map[string]string{
		"SYSLOG_IDENTIFIER": "nodexa-agent",
		"NODEXA_EVENT":      id,
	}
	for k, v := range fields {
		vars["NODEXA_"+journalVarName(k)] = v
	}

	if b.journal {
		if err := journal.Send(message, journal.PriInfo, vars); err == nil {
			return
		}
	}
	// Fallback for environments without journald (e.g. native `go run` on a
	// developer machine): plain stdout logging, still carrying the event ID.
	log.Printf("[%s] %s %v", id, message, fields)
}

// Recent returns up to n of the most recently emitted events, newest last.
func (b *Bus) Recent(n int) []Event {
	b.mu.Lock()
	defer b.mu.Unlock()

	if n <= 0 || n > len(b.buf) {
		n = len(b.buf)
	}
	out := make([]Event, n)
	copy(out, b.buf[len(b.buf)-n:])
	return out
}

// journalVarName maps a field key to journald's required variable-name
// alphabet (uppercase letters, digits, underscore -- see sd_journal_print(3))
// so fields like "overall"/"network" don't get silently dropped by
// go-systemd's validVarName check once prefixed with "NODEXA_".
func journalVarName(k string) string {
	var b strings.Builder
	for _, c := range strings.ToUpper(k) {
		if ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9') {
			b.WriteRune(c)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}

// Fieldsf is a small convenience for building single-field maps inline,
// e.g. events.Fieldsf("container", name).
func Fieldsf(key, format string, args ...interface{}) map[string]string {
	return map[string]string{key: fmt.Sprintf(format, args...)}
}
