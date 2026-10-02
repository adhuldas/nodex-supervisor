package container

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/events"
)

// superviseInterval is how often Supervise looks for exited containers.
const superviseInterval = 2 * time.Second

// Restart backoff, as in Docker: doubling from restartBackoffBase up to
// restartBackoffMax, reset once a container stays up for restartResetAfter.
const (
	restartBackoffBase = 1 * time.Second
	restartBackoffMax  = 1 * time.Minute
	restartResetAfter  = 30 * time.Second
)

type restartState struct {
	failures int
	nextAt   time.Time
}

// restarts reports whether a compose restart policy brings a container back
// after its process exits. runc doesn't keep the exit status, so
// "on-failure" is treated like "always".
func restarts(policy string) bool {
	switch policy {
	case "always", "unless-stopped", "on-failure":
		return true
	}
	return false
}

// Supervise restarts containers that exited on their own, per their restart
// policy, until ctx is done. A container stopped through Stop (dashboard,
// nodexactl, a redeploy) is never restarted: Stop deletes its runc state,
// while one whose process exited is still in runc as "stopped".
func (m *NodexaContainerManager) Supervise(ctx context.Context) {
	t := time.NewTicker(superviseInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.restartExited(time.Now())
		}
	}
}

func (m *NodexaContainerManager) restartExited(now time.Time) {
	entries, err := os.ReadDir(m.containerDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		md, err := readMetadata(filepath.Join(m.containerDir, name))
		if err != nil || !restarts(md.Restart) {
			m.forgetRestart(name)
			continue
		}
		// Mid-deploy: Create/Start own it.
		m.transientMu.Lock()
		_, busy := m.transient[name]
		m.transientMu.Unlock()
		if busy {
			continue
		}

		st, err := m.runner.State(name)
		if err != nil {
			continue // never started, or stopped on purpose
		}
		if st.Status != "stopped" {
			if !md.StartedAt.IsZero() && now.Sub(md.StartedAt) >= restartResetAfter {
				m.forgetRestart(name)
			}
			continue
		}

		m.restartMu.Lock()
		rs := m.restartState[name]
		if now.Before(rs.nextAt) {
			m.restartMu.Unlock()
			continue
		}
		rs.failures++
		delay := restartBackoffBase << uint(min(rs.failures-1, 10))
		rs.nextAt = now.Add(min(delay, restartBackoffMax))
		m.restartState[name] = rs
		m.restartMu.Unlock()

		log.Printf("container %s exited; restarting (restart: %s, attempt %d)", name, md.Restart, rs.failures)
		m.bus.Emit(events.ContainerFailed, "container exited, restarting", events.Fieldsf("container", "%s", name))
		if err := m.Start(name); err != nil {
			log.Printf("warning: restarting container %s: %v", name, err)
		}
	}
}

func (m *NodexaContainerManager) forgetRestart(name string) {
	m.restartMu.Lock()
	delete(m.restartState, name)
	m.restartMu.Unlock()
}
