// Package deploy applies Nodexa Deploy deployments fetched from
// nodexa-backend (internal/backend.DeploymentResponse) onto the local
// container runtime (internal/container). It owns the diffing logic that
// keeps a revision update from being a full stop-everything-and-restart:
// only services whose spec changed are pulled/recreated, and only
// containers this same deployment previously created but no longer wants
// are stopped.
package deploy

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/backend"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/container"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/events"
)

// retryBackoffBase and retryBackoffMax bound how quickly a persistently
// failing revision is retried. Without this, cmd/nodexa-agent's heartbeat
// loop calls Apply again on every tick (every HealthInterval, 30s by
// default) for as long as the same bad revision is assigned -- each attempt
// re-running a full skopeo pull + umoci unpack + runc run, which is real
// CPU/IO work, not a cheap no-op. That pegged CPU for minutes on a device
// stuck on a failing deployment (observed 2026-09-21). Backoff is
// exponential per consecutive failure of the *same* name+revision, so a
// fleet-wide bad rollout doesn't hammer every device's CPU indefinitely,
// while a fix (new revision, or the same revision after an operator retry)
// is still picked up on the very next heartbeat.
const (
	retryBackoffBase = 1 * time.Minute
	retryBackoffMax  = 15 * time.Minute
)

// Manager applies deployments onto a container.NodexaContainerManager.
type Manager struct {
	containers *container.NodexaContainerManager
	bus        *events.Bus

	mu              sync.Mutex
	applying        bool
	appliedName     string
	appliedRevision int

	failedName     string
	failedRevision int
	failureCount   int
	nextRetryAt    time.Time
}

// NewManager creates a deploy Manager. It starts with no applied
// deployment (AppliedRevision 0) -- on a fresh boot, cmd/nodexa-agent's
// heartbeat ticker learns the device's actual effective revision from its
// first heartbeat response, same as every revision change after that.
func NewManager(containers *container.NodexaContainerManager, bus *events.Bus) *Manager {
	return &Manager{containers: containers, bus: bus}
}

// AppliedName and AppliedRevision report the deployment last successfully
// applied (empty/0 if none yet). Used both by the heartbeat ticker (to
// decide whether a new revision needs fetching) and by the agent API's
// deployment status endpoint.
func (m *Manager) AppliedName() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.appliedName
}

// IsApplying reports whether a deployment is currently being applied.
func (m *Manager) IsApplying() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.applying
}

func (m *Manager) AppliedRevision() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.appliedRevision
}

// ForgetApplied makes the next heartbeat's revision look new, so it gets
// fetched and applied even when it matches the last applied number -- used
// after a fleet transfer, where the new fleet's revisions are unrelated to
// the old fleet's.
func (m *Manager) ForgetApplied() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appliedRevision = 0
}

// HasDeployed reports whether any container on the device was created by
// a deployment (as opposed to a seeded/demo one).
func (m *Manager) HasDeployed() bool {
	list, err := m.containers.List()
	if err != nil {
		return false
	}
	for _, c := range list {
		if c.DeploymentName != "" {
			return true
		}
	}
	return false
}

// RemoveAll stops and removes every container a deployment created,
// leaving the device with no release -- used when the cloud assigns none
// (e.g. after a fleet transfer to a fleet without releases). Containers not created by a
// deployment are left alone, and images still used by them are kept.
func (m *Manager) RemoveAll() error {
	m.mu.Lock()
	if m.applying {
		m.mu.Unlock()
		return fmt.Errorf("deploy: a deployment is already being applied")
	}
	m.applying = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.applying = false
		m.mu.Unlock()
	}()

	existing, err := m.containers.List()
	if err != nil {
		return fmt.Errorf("deploy: listing containers: %w", err)
	}
	var firstErr error
	keepImages := make(map[string]bool)
	for _, c := range existing {
		if c.DeploymentName == "" {
			keepImages[c.Image] = true
			continue
		}
		if err := m.containers.Remove(c.Name); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("removing container %q: %w", c.Name, err)
		}
	}
	_ = m.containers.PruneUnusedImages(keepImages)
	if firstErr != nil {
		return firstErr
	}

	m.mu.Lock()
	m.appliedName = ""
	m.appliedRevision = 0
	m.mu.Unlock()
	m.bus.Emit(events.DeploymentCompleted, "no release assigned, removed deployed containers", nil)
	return nil
}

// Apply reconciles the local container set against dep: it deploys
// (pulls, builds a bundle for, and starts) every service in dep.Services,
// then stops any container this same deployment previously created that
// is no longer part of the spec, and any still-running seeded/demo
// container (see container.NodexaContainerManager.SeededNames) not reused
// by dep.Services -- a real deployment always takes priority over a
// device's baked-in demo workload. Containers not owned by this
// deployment or seeded (a different deployment's containers -- see
// nodexa-backend's fleet/device-pinning override rule) are left alone.
//
// Only one Apply runs at a time: a slow image pull must not let a second,
// overlapping heartbeat tick start a concurrent Apply against the same
// bundle directories.
func (m *Manager) Apply(dep *backend.DeploymentResponse) error {
	m.mu.Lock()
	if m.applying {
		m.mu.Unlock()
		return fmt.Errorf("deploy: a deployment is already being applied")
	}
	if dep.Name == m.failedName && dep.Revision == m.failedRevision && time.Now().Before(m.nextRetryAt) {
		retryAt := m.nextRetryAt
		m.mu.Unlock()
		return fmt.Errorf("deploy: %q revision %d failed recently, backing off until %s", dep.Name, dep.Revision, retryAt.Format(time.RFC3339))
	}
	m.applying = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.applying = false
		m.mu.Unlock()
	}()

	// A real release now owns the device: retire the seeded demo
	// container(s) for good, before anything else -- even if this apply
	// later fails, or is skipped below because the release is already up.
	keep := make(map[string]bool, len(dep.Services))
	for _, svc := range dep.Services {
		keep[svc.Name] = true
	}
	if err := m.containers.RetireSeeds(keep); err != nil {
		m.bus.Emit(events.DeploymentFailed, "retiring seeded containers failed", events.Fieldsf("deployment", "%s", dep.Name))
	}

	// appliedRevision is in-memory only, so every agent restart (including
	// right after an agent OTA) re-applies the effective deployment -- and
	// containers.Create always stops and recreates. If this exact release
	// is already up, adopt it instead of restarting healthy workloads.
	if existing, err := m.containers.List(); err == nil && releaseRunning(dep, existing) {
		m.markApplied(dep)
		m.bus.Emit(events.DeploymentCompleted, "deployment already running, skipped apply", events.Fieldsf("deployment", "%s", dep.Name))
		return nil
	}

	m.bus.Emit(events.DeploymentStarted, "applying deployment", events.Fieldsf("deployment", "%s", dep.Name))

	creds := credentialsByRegistry(dep.Registries)

	wanted := make(map[string]bool, len(dep.Services))
	wantedImages := make(map[string]bool, len(dep.Services))

	// Create every service before starting any, so each one's name
	// resolves (DNS and /etc/hosts) the moment its siblings start: nginx,
	// for one, exits at startup on an upstream it can't resolve, and
	// compose's depends_on ordering isn't available here.
	var firstErr error
	var created []string
	for _, svc := range dep.Services {
		wanted[svc.Name] = true
		wantedImages[svc.Image] = true
		var cred *container.RegistryCredential
		if c, ok := creds[registryFor(svc.Image)]; ok {
			cred = &c
		}
		if err := m.containers.Create(svc, cred, dep.Name, dep.Revision); err != nil {
			m.bus.Emit(events.DeploymentFailed, "service deploy failed", events.Fieldsf("service", "%s", svc.Name))
			if firstErr == nil {
				firstErr = fmt.Errorf("service %q: %w", svc.Name, err)
			}
			continue
		}
		created = append(created, svc.Name)
	}
	for _, name := range created {
		if err := m.containers.Start(name); err != nil {
			m.bus.Emit(events.DeploymentFailed, "service deploy failed", events.Fieldsf("service", "%s", name))
			if firstErr == nil {
				firstErr = fmt.Errorf("service %q: %w", name, err)
			}
		}
	}

	// Remove any container on the device that is not part of the active release.
	// This cleans up old releases (different deployment names or old revisions),
	// removed services, and any seeded/demo container (nodexa-test).
	if existing, err := m.containers.List(); err == nil {
		for _, c := range existing {
			if wanted[c.Name] {
				continue
			}
			if err := m.containers.Remove(c.Name); err != nil && firstErr == nil {
				firstErr = fmt.Errorf("removing old container %q: %w", c.Name, err)
			}
		}
	}

	// Prune any cached images that are not used by the current deployment's services.
	_ = m.containers.PruneUnusedImages(wantedImages)

	if firstErr != nil {
		m.mu.Lock()
		if dep.Name == m.failedName && dep.Revision == m.failedRevision {
			m.failureCount++
		} else {
			m.failedName = dep.Name
			m.failedRevision = dep.Revision
			m.failureCount = 1
		}
		m.nextRetryAt = time.Now().Add(backoffDuration(m.failureCount))
		m.mu.Unlock()
		return firstErr
	}

	m.markApplied(dep)
	m.bus.Emit(events.DeploymentCompleted, "deployment applied", events.Fieldsf("deployment", "%s", dep.Name))
	return nil
}

// markApplied records dep as the applied deployment and clears any
// retry backoff state for it.
func (m *Manager) markApplied(dep *backend.DeploymentResponse) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appliedName = dep.Name
	m.appliedRevision = dep.Revision
	m.failedName = ""
	m.failedRevision = 0
	m.failureCount = 0
}

// releaseRunning reports whether every service in dep already has a
// running container created by this same deployment name and revision
// with the same image. A deployment with no services is never considered
// running, so Apply still gets to clean up stale containers for it.
func releaseRunning(dep *backend.DeploymentResponse, existing []container.NodexaContainer) bool {
	if len(dep.Services) == 0 {
		return false
	}
	byName := make(map[string]container.NodexaContainer, len(existing))
	for _, c := range existing {
		byName[c.Name] = c
	}
	for _, svc := range dep.Services {
		c, ok := byName[svc.Name]
		if !ok ||
			c.State != container.StateRunning ||
			c.DeploymentName != dep.Name ||
			c.DeploymentRevision != dep.Revision ||
			c.Image != svc.Image {
			return false
		}
	}
	return true
}

// backoffDuration returns the delay before the next retry of a revision
// that has now failed failureCount consecutive times: doubling from
// retryBackoffBase, capped at retryBackoffMax.
func backoffDuration(failureCount int) time.Duration {
	if failureCount < 1 {
		failureCount = 1
	}
	if failureCount > 10 { // avoid overflow; well past retryBackoffMax by then anyway
		failureCount = 10
	}
	d := retryBackoffBase << uint(failureCount-1)
	if d > retryBackoffMax {
		return retryBackoffMax
	}
	return d
}

// ownedByPreviousApply lists the names of currently known containers
// tagged as belonging to deploymentName -- read fresh from
// NodexaContainerManager.List rather than cached, so a container stopped
// or removed outside Apply (e.g. via nodexactl) is never double-stopped.
func (m *Manager) ownedByPreviousApply(deploymentName string) []string {
	list, err := m.containers.List()
	if err != nil {
		return nil
	}
	var out []string
	for _, c := range list {
		if c.DeploymentName == deploymentName {
			out = append(out, c.Name)
		}
	}
	return out
}

func credentialsByRegistry(regs []container.RegistryCredential) map[string]container.RegistryCredential {
	out := make(map[string]container.RegistryCredential, len(regs))
	for _, r := range regs {
		out[r.Registry] = r
	}
	return out
}

// registryFor extracts the registry host from an image reference, using
// the same rule `docker pull`/skopeo use: a reference's first path segment
// is a registry host only if it contains a "." or ":" (a domain, or a
// domain:port), or is exactly "localhost" -- otherwise the whole reference
// is Docker Hub shorthand.
func registryFor(image string) string {
	firstSlash := strings.IndexByte(image, '/')
	if firstSlash < 0 {
		return "docker.io"
	}
	first := image[:firstSlash]
	if first == "localhost" || strings.ContainsAny(first, ".:") {
		return first
	}
	return "docker.io"
}
