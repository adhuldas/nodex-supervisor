// Package watchdog integrates nodexa-agent with systemd's service watchdog
// (WatchdogSec=) and readiness protocol (Type=notify). If the agent's main
// loop hangs or a self-check fails, nodexa-agent simply stops pinging the
// watchdog; systemd then kills and restarts the unit per
// nodexa-agent.service's Restart= policy. This is the "agent crash
// recovery" mechanism validated by the QEMU integration tests.
package watchdog

import (
	"context"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"
)

// HealthFunc reports whether the agent is currently healthy enough to keep
// petting the watchdog.
type HealthFunc func() bool

// Watchdog pings systemd on an interval derived from the unit's
// WatchdogSec= setting.
type Watchdog struct {
	interval time.Duration
	enabled  bool
}

// New inspects the WATCHDOG_USEC/WATCHDOG_PID environment set by systemd
// (present only when the unit has WatchdogSec= configured) and derives a
// safe ping interval (half the configured timeout, per sd_watchdog_enabled
// guidance).
func New() *Watchdog {
	usec, err := daemon.SdWatchdogEnabled(false)
	if err != nil || usec == 0 {
		return &Watchdog{enabled: false}
	}
	return &Watchdog{interval: usec / 2, enabled: true}
}

// Enabled reports whether systemd has configured a watchdog timeout for
// this unit.
func (w *Watchdog) Enabled() bool { return w.enabled }

// NotifyReady tells systemd the agent has finished initializing. Required
// for nodexa-agent.service's Type=notify to consider the unit started.
func NotifyReady() {
	_, _ = daemon.SdNotify(false, daemon.SdNotifyReady)
}

// NotifyStopping tells systemd the agent is beginning a graceful shutdown.
func NotifyStopping() {
	_, _ = daemon.SdNotify(false, daemon.SdNotifyStopping)
}

// Run pings the watchdog on its interval for as long as healthy() returns
// true, or until ctx is cancelled. It blocks; call it in its own goroutine.
func (w *Watchdog) Run(ctx context.Context, healthy HealthFunc) {
	if !w.enabled {
		return
	}
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if healthy == nil || healthy() {
				_, _ = daemon.SdNotify(false, daemon.SdNotifyWatchdog)
			}
			// If unhealthy, deliberately skip the ping: systemd's
			// WatchdogSec= timeout will expire and restart the unit.
		}
	}
}
