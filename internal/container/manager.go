package container

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/events"
)

// NodexaContainerManager manages the lifecycle of locally defined
// containers on top of runc. Phase 1 only started/stopped/listed bundles
// that already existed under its container directory; Create (below) adds
// pulling images and generating those bundles on the fly for Nodexa
// Deploy, layered on top of that same foundation.
type NodexaContainerManager struct {
	mu sync.Mutex

	containerDir  string
	seedDir       string
	imageCacheDir string
	volumesDir    string
	runner        *execRunner
	bus           *events.Bus
	netMgr        *NetworkManager
	// deviceEnv is set in every container's environment (e.g. DEVICE_ID),
	// over whatever the image or deployment set for the same names.
	deviceEnv map[string]string

	transientMu   sync.Mutex
	transient     map[string]NodexaContainer
	onStateChange func()

	// Supervise's per-container restart backoff (restart.go).
	restartMu    sync.Mutex
	restartState map[string]restartState

	engine EngineType
}

// NewManager creates a container manager.
//
//	containerDir:  writable directory holding live OCI bundles (typically
//	               /var/lib/nodexa/containers)
//	seedDir:       optional read-only directory of reference bundles baked
//	               into the image (typically /usr/share/nodexa/containers);
//	               used only to seed containerDir on first boot
//	imageCacheDir: local OCI-layout cache Create pulls images into before
//	               unpacking them (typically /var/lib/nodexa/image-cache)
//	volumesDir:    root directory for Nodexa-managed named volumes,
//	               kept outside containerDir so a redeploy's fresh bundle
//	               unpack doesn't wipe volume data (typically
//	               /var/lib/nodexa/volumes)
//	runcRoot:      directory runc uses for its own transient state
//	               (typically /run/nodexa/runc)
func NewManager(containerDir, seedDir, imageCacheDir, volumesDir, runcPath, runcRoot string, bus *events.Bus) *NodexaContainerManager {
	return &NodexaContainerManager{
		containerDir:  containerDir,
		seedDir:       seedDir,
		imageCacheDir: imageCacheDir,
		volumesDir:    volumesDir,
		runner:        newExecRunner(runcPath, runcRoot),
		bus:           bus,
		transient:     make(map[string]NodexaContainer),
		restartState:  make(map[string]restartState),
		engine:        EngineRunc,
	}
}

// SetEngine sets the active container runtime engine (runc, docker, nerdctl).
func (m *NodexaContainerManager) SetEngine(engine EngineType) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.engine = engine
}

func (m *NodexaContainerManager) engineLocked() EngineType {
	if m.engine == "" {
		return EngineRunc
	}
	return m.engine
}

// Engine returns the active container runtime engine.
func (m *NodexaContainerManager) Engine() EngineType {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.engineLocked()
}

// SetDeviceEnv sets variables every container gets, applied on each start
// so bundles created before this agent version get them too.
func (m *NodexaContainerManager) SetDeviceEnv(env map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deviceEnv = env
}

// SetNetworkManager wires the container network manager into the container manager.
func (m *NodexaContainerManager) SetNetworkManager(netMgr *NetworkManager) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.netMgr = netMgr
	m.restoreNetworkLocked(func(name string) bool {
		st, err := m.runner.State(name)
		return err == nil && st.Status == "running"
	})
}

// restoreNetworkLocked rebuilds netMgr's in-memory view of the containers
// still running under runc, which outlive an agent restart: their IPs are
// reserved and they're registered for name resolution again. Otherwise a
// container restarted later would get a /etc/hosts and DNS without its
// running siblings, and could even be handed one of their IPs.
func (m *NodexaContainerManager) restoreNetworkLocked(isRunning func(name string) bool) {
	entries, _ := os.ReadDir(m.containerDir)
	networks := map[string]bool{}
	for _, e := range entries {
		md, err := readMetadata(filepath.Join(m.containerDir, e.Name()))
		if err != nil || md.IPAddress == "" || md.Network == "host" || md.Network == "none" || !isRunning(e.Name()) {
			continue
		}
		netName := md.Network
		if netName == "" {
			netName = "bridge"
		}
		if !m.netMgr.ReserveContainerIP(e.Name(), md.IPAddress) {
			log.Printf("warning: container %s: IP %s is also used by another container", e.Name(), md.IPAddress)
		}
		m.netMgr.RegisterContainer(netName, e.Name(), md.IPAddress, md.Aliases, md.Ports)
		networks[netName] = true
	}
	for netName := range networks {
		m.netMgr.SyncNetworkHosts(netName, m.containerDir)
	}
}

// SetOnStateChange registers a callback invoked whenever a container's
// lifecycle state transitions (e.g. pulling -> installing -> created -> running).
func (m *NodexaContainerManager) SetOnStateChange(fn func()) {
	m.transientMu.Lock()
	m.onStateChange = fn
	m.transientMu.Unlock()
}

func (m *NodexaContainerManager) setTransient(c NodexaContainer) {
	m.transientMu.Lock()
	m.transient[c.Name] = c
	cb := m.onStateChange
	m.transientMu.Unlock()

	if cb != nil {
		cb()
	}
}

func (m *NodexaContainerManager) clearTransient(name string) {
	m.transientMu.Lock()
	delete(m.transient, name)
	cb := m.onStateChange
	m.transientMu.Unlock()

	if cb != nil {
		cb()
	}
}

// Ready reports whether the container runtime (runc or docker) is available. Used as
// the health.RuntimeCheckFunc.
func (m *NodexaContainerManager) Ready() bool {
	if m.Engine() == EngineDocker {
		return IsDockerAvailable()
	}
	if m.Engine() == EngineNerdctl {
		return IsNerdctlAvailable()
	}
	return m.runner.Available()
}

// Bootstrap seeds containerDir with any reference bundles from seedDir that
// are not already present. It never overwrites an existing bundle, so
// operator or deployment changes to a running container's bundle are never
// clobbered by a reboot.
// EnsureStorageExpanded attempts an online filesystem resize of the container
// storage partition (typically /dev/disk/by-label/nodexa-data mounted at
// /var/lib/nodexa) so containers always have access to 100% of the underlying
// partition's physical space.
func (m *NodexaContainerManager) EnsureStorageExpanded() {
	target := "/dev/disk/by-label/nodexa-data"
	if _, err := os.Stat(target); err != nil {
		return
	}

	cmd := exec.Command("resize2fs", target)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("container: notice: resizing storage partition %s: %v (%s)", target, err, strings.TrimSpace(string(out)))
	} else if strings.Contains(string(out), "The filesystem on") {
		log.Printf("container: storage partition %s expanded: %s", target, strings.TrimSpace(string(out)))
	}
}

func (m *NodexaContainerManager) Bootstrap() error {
	m.EnsureStorageExpanded()

	if m.seedDir == "" || m.SeedsRetired() {
		return nil
	}
	// Devices that got a release before the seed-retired marker existed:
	// a deployment-owned bundle on disk means seeds are already done with.
	if m.hasDeploymentBundle() {
		return m.writeSeedRetired()
	}
	entries, err := os.ReadDir(m.seedDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("container: reading seed dir: %w", err)
	}

	if err := os.MkdirAll(m.containerDir, 0o700); err != nil {
		return err
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dst := filepath.Join(m.containerDir, e.Name())
		if _, err := os.Stat(dst); err == nil {
			continue // already seeded (or user-managed) - leave it alone
		}
		if err := copyDir(filepath.Join(m.seedDir, e.Name()), dst); err != nil {
			return fmt.Errorf("container: seeding %s: %w", e.Name(), err)
		}
	}
	return nil
}

// seedRetiredFile marks that a real deployment has taken over this device,
// so seeded/demo bundles are never copied in or started again. It lives
// next to containerDir on the data partition (/var/lib/nodexa), which only
// a fresh flash recreates -- reboots and agent/OS updates keep it.
const seedRetiredFile = "seed-retired"

func (m *NodexaContainerManager) seedRetiredPath() string {
	return filepath.Join(filepath.Dir(m.containerDir), seedRetiredFile)
}

// SeedsRetired reports whether seeded containers have been permanently
// retired by a deployment (see RetireSeeds).
func (m *NodexaContainerManager) SeedsRetired() bool {
	_, err := os.Stat(m.seedRetiredPath())
	return err == nil
}

func (m *NodexaContainerManager) writeSeedRetired() error {
	if err := os.WriteFile(m.seedRetiredPath(), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		return fmt.Errorf("container: writing seed-retired marker: %w", err)
	}
	return nil
}

// hasDeploymentBundle reports whether any bundle in containerDir was
// created by a deployment.
func (m *NodexaContainerManager) hasDeploymentBundle() bool {
	entries, err := os.ReadDir(m.containerDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if md, err := readMetadata(filepath.Join(m.containerDir, e.Name())); err == nil && md.DeploymentName != "" {
			return true
		}
	}
	return false
}

// RetireSeeds permanently retires seeded/demo containers once a real
// deployment is assigned: it records the seed-retired marker first (so an
// interrupted removal still never re-seeds) and then removes every seeded
// bundle not named in keep.
func (m *NodexaContainerManager) RetireSeeds(keep map[string]bool) error {
	if !m.SeedsRetired() {
		if err := m.writeSeedRetired(); err != nil {
			return err
		}
	}
	seeded, err := m.SeededNames()
	if err != nil {
		return err
	}
	for _, name := range seeded {
		if keep[name] {
			continue
		}
		if err := m.Remove(name); err != nil {
			return err
		}
	}
	return nil
}

// SeededNames returns the names of every reference bundle shipped in
// seedDir (e.g. "nodexa-test"), regardless of whether Bootstrap has copied
// it into containerDir yet. cmd/nodexa-agent's main uses this to
// auto-start seeded containers at boot -- so a freshly flashed device
// shows visible activity even before any Nodexa Deploy deployment is
// assigned -- and deploy.Manager.Apply uses it to stop them again the
// moment a real deployment lands, since a seeded/demo container is never
// deployment-owned and Apply otherwise leaves non-owned containers alone.
func (m *NodexaContainerManager) SeededNames() ([]string, error) {
	if m.seedDir == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(m.seedDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("container: reading seed dir: %w", err)
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// List returns every known container, reconciling on-disk metadata with
// runc's live view of running processes, and including in-flight transient
// states (pulling image, installing image, container created).
func (m *NodexaContainerManager) List() ([]NodexaContainer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	eng := m.engineLocked()
	if eng == EngineDocker || eng == EngineNerdctl {
		list, err := dockerList(context.Background(), eng.cliBinary(), m.containerDir)
		if err != nil {
			return nil, err
		}
		m.transientMu.Lock()
		for _, tc := range m.transient {
			list = append(list, tc)
		}
		m.transientMu.Unlock()
		sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
		return list, nil
	}

	entries, err := os.ReadDir(m.containerDir)
	if os.IsNotExist(err) {
		entries = nil
	} else if err != nil {
		return nil, err
	}

	seen := make(map[string]bool)
	var out []NodexaContainer

	// Include in-flight transient containers first
	m.transientMu.Lock()
	for name, tc := range m.transient {
		out = append(out, tc)
		seen[name] = true
	}
	m.transientMu.Unlock()

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if seen[e.Name()] {
			continue
		}
		bundleDir := filepath.Join(m.containerDir, e.Name())
		md, err := readMetadata(bundleDir)
		if err != nil {
			continue // not a valid Nodexa bundle; skip
		}
		out = append(out, m.reconcile(e.Name(), md))
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get returns a single container by name, reconciling metadata and live state.
func (m *NodexaContainerManager) Get(name string) (*NodexaContainer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.transientMu.Lock()
	if tc, ok := m.transient[name]; ok {
		m.transientMu.Unlock()
		return &tc, nil
	}
	m.transientMu.Unlock()

	eng := m.engineLocked()
	if eng == EngineDocker || eng == EngineNerdctl {
		return dockerInspect(context.Background(), eng.cliBinary(), name, m.containerDir)
	}

	bundleDir := filepath.Join(m.containerDir, name)
	md, err := readMetadata(bundleDir)
	if err != nil {
		return nil, fmt.Errorf("container %q not found", name)
	}

	c := m.reconcile(name, md)
	return &c, nil
}

// reconcile merges persisted metadata with runc's live state for one
// container.
func (m *NodexaContainerManager) reconcile(name string, md *metadata) NodexaContainer {
	c := NodexaContainer{
		Name:               name,
		Image:              md.Image,
		Command:            md.Command,
		CreatedAt:          md.CreatedAt,
		StartedAt:          md.StartedAt,
		State:              StateCreated,
		DeploymentName:     md.DeploymentName,
		DeploymentRevision: md.DeploymentRevision,
		Network:            md.Network,
		IPAddress:          md.IPAddress,
	}

	st, err := m.runner.State(name)
	if err != nil {
		// runc has no record of this container: it has never been
		// started, or it was cleaned up after stopping.
		if !md.StartedAt.IsZero() {
			c.State = StateStopped
		}
		return c
	}

	c.PID = st.Pid
	switch st.Status {
	case "running", "created":
		c.State = StateRunning
	default:
		c.State = StateStopped
	}
	return c
}

// Start launches a container by name (its directory name under
// containerDir). It is idempotent: starting an already-running container is
// a no-op that still returns success.
func (m *NodexaContainerManager) Start(name string) error {
	defer m.clearTransient(name)
	m.mu.Lock()
	defer m.mu.Unlock()

	eng := m.engineLocked()
	if eng == EngineDocker || eng == EngineNerdctl {
		cli := eng.cliBinary()
		if err := dockerStart(context.Background(), cli, name); err != nil {
			m.bus.Emit(events.ContainerFailed, "container failed to start", events.Fieldsf("container", "%s", name))
			return err
		}
		bundleDir := filepath.Join(m.containerDir, name)
		if md, _ := readMetadata(bundleDir); md != nil {
			md.StartedAt = time.Now().UTC()
			_ = writeMetadata(bundleDir, md)
		}
		m.bus.Emit(events.ContainerStarted, "container started", events.Fieldsf("container", "%s", name))
		if m.onStateChange != nil {
			m.onStateChange()
		}
		return nil
	}
	return m.startLocked(name)
}

// startLocked is Start's body; the caller holds m.mu.
func (m *NodexaContainerManager) startLocked(name string) error {
	bundleDir := filepath.Join(m.containerDir, name)
	md, err := readMetadata(bundleDir)
	if err != nil {
		return fmt.Errorf("container: unknown container %q: %w", name, err)
	}

	if st, err := m.runner.State(name); err == nil && st.Status == "running" {
		return nil // already running
	}

	if _, err := readBundleSpec(bundleDir); err != nil {
		return err
	}

	if md.Network != "host" && md.Network != "none" && m.netMgr != nil {
		netName := md.Network
		if netName == "" {
			netName = "bridge"
		}
		netObj, err := m.netMgr.EnsureNetwork(netName)
		if err == nil {
			netnsName := "nodexa-" + name
			netnsPath := "/run/netns/" + netnsName
			if md.IPAddress != "" {
				m.netMgr.ReserveContainerIP(name, md.IPAddress)
			}
			if !isNetnsMount(netnsPath) {
				_, ip, err := m.netMgr.SetupContainerNetwork(name, netObj)
				if err != nil {
					return fmt.Errorf("container: starting %q: setting up network %q: %w", name, netName, err)
				}
				// Differs from the saved IP only if another container
				// took that one while this one was stopped.
				if ip != "" {
					md.IPAddress = ip
				}
			} else {
				m.netMgr.EnsureContainerBridgeAttached(name, netObj.Bridge)
			}
			m.netMgr.RegisterContainer(netObj.Name, name, md.IPAddress, md.Aliases, md.Ports)
			m.netMgr.SyncNetworkHosts(netObj.Name, m.containerDir)
		}
	}

	if md.Network == "host" {
		// Also refreshes bundles created before host-network siblings
		// became resolvable.
		m.syncHostNetworkHostsLocked()
	}

	if err := applyEnv(bundleDir, m.deviceEnv); err != nil {
		return fmt.Errorf("container: starting %q: %w", name, err)
	}

	if err := m.runner.Run(bundleDir, name); err != nil {
		m.bus.Emit(events.ContainerFailed, "container failed to start", events.Fieldsf("container", "%s", name))
		return fmt.Errorf("container: starting %q: %w", name, err)
	}

	md.StartedAt = time.Now().UTC()
	if err := writeMetadata(bundleDir, md); err != nil {
		return err
	}

	m.bus.Emit(events.ContainerStarted, "container started", events.Fieldsf("container", "%s", name))
	return nil
}

// Stop stops a running container (SIGTERM, then removes runc's runtime
// state). It is idempotent: stopping an already-stopped container is a
// no-op that still returns success.
func (m *NodexaContainerManager) Stop(name string) error {
	m.clearTransient(name)
	m.mu.Lock()
	defer m.mu.Unlock()

	eng := m.engineLocked()
	if eng == EngineDocker || eng == EngineNerdctl {
		cli := eng.cliBinary()
		_ = dockerStop(context.Background(), cli, name)
		m.bus.Emit(events.ContainerStopped, "container stopped", events.Fieldsf("container", "%s", name))
		if m.onStateChange != nil {
			m.onStateChange()
		}
		return nil
	}

	bundleDir := filepath.Join(m.containerDir, name)
	md, _ := readMetadata(bundleDir)

	if _, err := m.runner.State(name); err == nil {
		_ = m.runner.Kill(name, "KILL")
		_ = m.runner.Delete(name)
	}

	if m.netMgr != nil {
		_ = m.netMgr.TeardownContainerNetwork(name)
		if md != nil && md.Network != "host" && md.Network != "none" {
			netName := md.Network
			if netName == "" {
				netName = "bridge"
			}
			m.netMgr.SyncNetworkHosts(netName, m.containerDir)
		}
	}

	m.bus.Emit(events.ContainerStopped, "container stopped", events.Fieldsf("container", "%s", name))
	return nil
}

// Remove stops name (if running) and then deletes its bundle directory and
// any Nodexa-managed volumes entirely, so it no longer appears in List at
// all -- unlike Stop, which only tears down runc's runtime state and leaves
// the bundle (and its metadata) on disk. Used when a superseded seeded/demo
// container (e.g. nodexa-test) must leave no trace once a real deployment
// takes over, rather than lingering as a "Created"/stopped entry in
// `nodexactl ps` forever. It is idempotent: removing an already-removed or
// never-seeded container is a no-op that still returns success.
func (m *NodexaContainerManager) Remove(name string) error {
	m.clearTransient(name)
	m.mu.Lock()
	defer m.mu.Unlock()

	eng := m.engineLocked()
	if eng == EngineDocker || eng == EngineNerdctl {
		cli := eng.cliBinary()
		_ = dockerRemove(context.Background(), cli, name)
		bundleDir := filepath.Join(m.containerDir, name)
		_ = os.RemoveAll(bundleDir)
		if m.onStateChange != nil {
			m.onStateChange()
		}
		return nil
	}

	bundleDir := filepath.Join(m.containerDir, name)
	md, _ := readMetadata(bundleDir)

	// Kill and force-delete any active or stopped container in runc
	_ = m.runner.Kill(name, "KILL")
	_ = m.runner.Delete(name)

	if m.netMgr != nil {
		_ = m.netMgr.TeardownContainerNetwork(name)
		if md != nil && md.Network != "host" && md.Network != "none" {
			netName := md.Network
			if netName == "" {
				netName = "bridge"
			}
			m.netMgr.SyncNetworkHosts(netName, m.containerDir)
		}
	}

	if err := os.RemoveAll(bundleDir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("container: removing %q: %w", name, err)
	}
	if err := os.RemoveAll(filepath.Join(m.volumesDir, name)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("container: removing %q: volumes: %w", name, err)
	}

	m.bus.Emit(events.ContainerRemoved, "container removed", events.Fieldsf("container", "%s", name))
	return nil
}

// Create pulls spec.Image (via skopeo, authenticating with creds if given),
// unpacks it into a fresh OCI bundle (via umoci), rewrites config.json for
// spec's command/env/mounts/volumes/network, and tags the bundle with the
// owning deployment's name and revision, without starting it. Its network
// endpoint is registered already, so a sibling started before it can
// resolve its name: that's why internal/deploy creates every service of a
// release before starting any (nginx exits at startup on an upstream it
// can't resolve). It emits structured transient states ("pulling image",
// "installing image", "container created") so periodic heartbeats reflect
// granular progress; Start clears them.
func (m *NodexaContainerManager) Create(spec ServiceSpec, creds *RegistryCredential, deploymentName string, deploymentRevision int) error {
	name := spec.Name
	bundleDir := filepath.Join(m.containerDir, name)
	volumesDir := filepath.Join(m.volumesDir, name)

	// Stop any existing instance so network ports, sockets, and cgroups are freed.
	_ = m.Stop(name)

	if m.Engine() == EngineDocker || m.Engine() == EngineNerdctl {
		cli := m.Engine().cliBinary()
		ctx := context.Background()

		m.setTransient(NodexaContainer{
			Name:               name,
			Image:              spec.Image,
			Command:            spec.Command,
			State:              StatePullingImage,
			CreatedAt:          time.Now().UTC(),
			DeploymentName:     deploymentName,
			DeploymentRevision: deploymentRevision,
		})
		m.bus.Emit(events.DeploymentProgress, "pulling image", map[string]string{
			"service": name,
			"image":   spec.Image,
			"state":   string(StatePullingImage),
		})

		if err := dockerPull(ctx, cli, spec.Image, creds); err != nil {
			m.clearTransient(name)
			return fmt.Errorf("container: deploying %q: %w", name, err)
		}

		m.setTransient(NodexaContainer{
			Name:               name,
			Image:              spec.Image,
			Command:            spec.Command,
			State:              StateInstallingImage,
			CreatedAt:          time.Now().UTC(),
			DeploymentName:     deploymentName,
			DeploymentRevision: deploymentRevision,
		})
		m.bus.Emit(events.DeploymentProgress, "installing image", map[string]string{
			"service": name,
			"image":   spec.Image,
			"state":   string(StateInstallingImage),
		})

		_ = os.MkdirAll(bundleDir, 0755)

		if err := dockerCreate(ctx, cli, spec, deploymentName, deploymentRevision, m.deviceEnv); err != nil {
			m.clearTransient(name)
			return fmt.Errorf("container: deploying %q: %w", name, err)
		}

		md := &metadata{
			Image:              spec.Image,
			Command:            spec.Command,
			CreatedAt:          time.Now().UTC(),
			DeploymentName:     deploymentName,
			DeploymentRevision: deploymentRevision,
			Restart:            spec.Restart,
		}
		_ = writeMetadata(bundleDir, md)

		m.setTransient(NodexaContainer{
			Name:               name,
			Image:              spec.Image,
			Command:            spec.Command,
			State:              StateContainerCreated,
			CreatedAt:          time.Now().UTC(),
			DeploymentName:     deploymentName,
			DeploymentRevision: deploymentRevision,
		})
		m.bus.Emit(events.DeploymentProgress, "container created", map[string]string{
			"service": name,
			"image":   spec.Image,
			"state":   string(StateContainerCreated),
		})

		time.Sleep(300 * time.Millisecond)
		return nil
	}

	// Phase 1: pulling image
	m.setTransient(NodexaContainer{
		Name:               name,
		Image:              spec.Image,
		Command:            spec.Command,
		State:              StatePullingImage,
		CreatedAt:          time.Now().UTC(),
		DeploymentName:     deploymentName,
		DeploymentRevision: deploymentRevision,
	})
	m.bus.Emit(events.DeploymentProgress, "pulling image", map[string]string{
		"service": name,
		"image":   spec.Image,
		"state":   string(StatePullingImage),
	})

	if err := pullImage(m.imageCacheDir, spec.Image, creds); err != nil {
		m.clearTransient(name)
		return fmt.Errorf("container: deploying %q: %w", name, err)
	}

	// Phase 2: installing image
	m.setTransient(NodexaContainer{
		Name:               name,
		Image:              spec.Image,
		Command:            spec.Command,
		State:              StateInstallingImage,
		CreatedAt:          time.Now().UTC(),
		DeploymentName:     deploymentName,
		DeploymentRevision: deploymentRevision,
	})
	m.bus.Emit(events.DeploymentProgress, "installing image", map[string]string{
		"service": name,
		"image":   spec.Image,
		"state":   string(StateInstallingImage),
	})

	existingMd, _ := readMetadata(bundleDir)
	needsUnpack := true
	if existingMd != nil && existingMd.Image == spec.Image {
		if _, err := os.Stat(filepath.Join(bundleDir, "rootfs")); err == nil {
			needsUnpack = false
		}
	}

	if needsUnpack {
		if err := unpackImage(m.imageCacheDir, bundleDir, spec.Image); err != nil {
			m.clearTransient(name)
			return fmt.Errorf("container: deploying %q: %w", name, err)
		}
	}

	var netnsPath string
	var containerIP string
	var gatewayIP string
	var aliases []string
	if spec.ContainerName != "" && spec.ContainerName != name {
		aliases = append(aliases, spec.ContainerName)
	}
	networkMode := spec.Network
	if networkMode == "" {
		networkMode = "bridge"
	}

	if networkMode != "host" && networkMode != "none" && m.netMgr != nil {
		netObj, err := m.netMgr.EnsureNetwork(networkMode)
		if err != nil {
			m.clearTransient(name)
			return fmt.Errorf("container: deploying %q: ensuring network %q: %w", name, networkMode, err)
		}
		ns, ip, err := m.netMgr.SetupContainerNetwork(name, netObj)
		if err != nil {
			m.clearTransient(name)
			return fmt.Errorf("container: deploying %q: setting up network %q: %w", name, networkMode, err)
		}
		netnsPath = ns
		containerIP = ip
		gatewayIP = netObj.Gateway

		m.netMgr.RegisterContainer(netObj.Name, name, ip, aliases, spec.Ports)
	}

	m.mu.Lock()
	if err := writeBundleConfig(bundleDir, spec, volumesDir, netnsPath, containerIP, gatewayIP); err != nil {
		m.clearTransient(name)
		m.mu.Unlock()
		return fmt.Errorf("container: deploying %q: writing config: %w", name, err)
	}

	md := &metadata{
		Name:               name,
		Image:              spec.Image,
		Command:            spec.Command,
		CreatedAt:          time.Now().UTC(),
		CgroupPath:         "nodexa/" + name,
		DeploymentName:     deploymentName,
		DeploymentRevision: deploymentRevision,
		Network:            networkMode,
		IPAddress:          containerIP,
		Ports:              spec.Ports,
		Aliases:            aliases,
		Restart:            spec.Restart,
	}
	if err := writeMetadata(bundleDir, md); err != nil {
		m.clearTransient(name)
		m.mu.Unlock()
		return fmt.Errorf("container: deploying %q: writing metadata: %w", name, err)
	}
	m.mu.Unlock()

	if m.netMgr != nil && networkMode != "host" && networkMode != "none" {
		m.netMgr.SyncNetworkHosts(networkMode, m.containerDir)
	}
	if networkMode == "host" {
		m.mu.Lock()
		m.syncHostNetworkHostsLocked()
		m.mu.Unlock()
	}

	// Phase 3: container created
	m.setTransient(NodexaContainer{
		Name:               name,
		Image:              spec.Image,
		Command:            spec.Command,
		State:              StateContainerCreated,
		CreatedAt:          md.CreatedAt,
		DeploymentName:     deploymentName,
		DeploymentRevision: deploymentRevision,
	})
	m.bus.Emit(events.DeploymentProgress, "container created", map[string]string{
		"service": name,
		"image":   spec.Image,
		"state":   string(StateContainerCreated),
	})

	// Give heartbeat ticker an opportunity to catch the created state
	time.Sleep(300 * time.Millisecond)
	return nil
}

// Stats returns current resource usage for a running container.
func (m *NodexaContainerManager) Stats(name string) (NodexaContainerStats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// engineLocked, not Engine(): m.mu is already held and isn't reentrant.
	if eng := m.engineLocked(); eng == EngineDocker || eng == EngineNerdctl {
		return dockerStats(context.Background(), eng.cliBinary(), name)
	}

	bundleDir := filepath.Join(m.containerDir, name)
	spec, err := readBundleSpec(bundleDir)
	if err != nil {
		return NodexaContainerStats{}, err
	}

	// runc resolves a relative cgroupsPath against its own cgroup's parent
	// (/system.slice/nodexa/<name> under systemd), not the cgroup root, so
	// ask the kernel where the container's init actually is.
	cgroupPath := spec.Linux.CgroupsPath
	if st, err := m.runner.State(name); err == nil && st.Status == "running" && st.Pid > 0 {
		if p := processCgroup(st.Pid); p != "" {
			cgroupPath = p
		}
	}
	cpu, mem, limit, pids := readCgroupStats(cgroupPath)
	return NodexaContainerStats{
		Name:             name,
		CPUUsageSeconds:  cpu,
		MemoryUsageBytes: mem,
		MemoryLimitBytes: limit,
		PIDs:             pids,
		Timestamp:        time.Now().UTC(),
	}, nil
}

// DiskUsage returns the bytes a container's bundle (its unpacked image
// and anything written into it) takes up. It walks the whole tree, so
// callers should cache it.
func (m *NodexaContainerManager) DiskUsage(name string) (uint64, error) {
	var total uint64
	err := filepath.WalkDir(filepath.Join(m.containerDir, name), func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // vanished mid-walk; best-effort like Stats
		}
		if d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				total += uint64(info.Size())
			}
		}
		return nil
	})
	return total, err
}

// Exec runs a command inside a running container and returns stdout, stderr, and exit code.
func (m *NodexaContainerManager) Exec(name string, cmd []string, user, cwd string) (string, string, int, error) {
	if m.Engine() == EngineDocker || m.Engine() == EngineNerdctl {
		return dockerExec(context.Background(), m.Engine().cliBinary(), name, cmd, user, cwd)
	}
	m.mu.Lock()
	bundleDir := filepath.Join(m.containerDir, name)
	if _, err := readMetadata(bundleDir); err != nil {
		m.mu.Unlock()
		return "", "", -1, fmt.Errorf("container: unknown container %q: %w", name, err)
	}
	m.mu.Unlock()

	st, err := m.runner.State(name)
	if err != nil || st.Status != "running" {
		return "", "", -1, fmt.Errorf("container %q is not running", name)
	}

	return m.runner.Exec(name, cmd, user, cwd)
}

// Logs returns the container's captured stdout/stderr.
func (m *NodexaContainerManager) Logs(name string, tail int) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// engineLocked, not Engine(): m.mu is already held and isn't reentrant.
	if eng := m.engineLocked(); eng == EngineDocker || eng == EngineNerdctl {
		return dockerLogs(context.Background(), eng.cliBinary(), name, tail)
	}

	bundleDir := filepath.Join(m.containerDir, name)
	if _, err := readMetadata(bundleDir); err != nil {
		return "", fmt.Errorf("container: unknown container %q: %w", name, err)
	}

	b, err := os.ReadFile(filepath.Join(bundleDir, "container.log"))
	if os.IsNotExist(err) {
		return "", nil // never started, or no output written yet
	}
	if err != nil {
		return "", fmt.Errorf("container: reading logs for %q: %w", name, err)
	}

	if tail <= 0 {
		return string(b), nil
	}
	return LastLines(string(b), tail), nil
}

// LogFilePath returns the path to name's captured stdout/stderr log file,
// after verifying the container exists. Exposed (rather than only Logs's
// whole/tail-limited read) so a caller streaming a "follow" response -- see
// internal/api's handleContainerLogsFollow -- can poll the file directly
// without holding the manager's lock for the lifetime of a long-running
// stream.
func (m *NodexaContainerManager) LogFilePath(name string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	bundleDir := filepath.Join(m.containerDir, name)
	if _, err := readMetadata(bundleDir); err != nil {
		return "", fmt.Errorf("container: unknown container %q: %w", name, err)
	}
	return filepath.Join(bundleDir, "container.log"), nil
}

// LastLines returns at most n trailing lines of s, preserving a trailing
// newline if present in the original text.
func LastLines(s string, n int) string {
	trimmed := strings.TrimSuffix(s, "\n")
	lines := strings.Split(trimmed, "\n")
	if len(lines) <= n {
		return s
	}
	out := strings.Join(lines[len(lines)-n:], "\n")
	if strings.HasSuffix(s, "\n") {
		out += "\n"
	}
	return out
}

// copyDir recursively copies a bundle directory, preserving the executable
// bit but not attempting to preserve every permission bit (bundles are
// world-readable reference data on a read-only image partition).
func copyDir(src, dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		srcPath := filepath.Join(src, e.Name())
		dstPath := filepath.Join(dst, e.Name())

		info, err := e.Info()
		if err != nil {
			return err
		}

		if info.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
			continue
		}

		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(srcPath)
			if err != nil {
				return err
			}
			if err := os.Symlink(target, dstPath); err != nil && !os.IsExist(err) {
				return err
			}
			continue
		}

		if err := copyFile(srcPath, dstPath, info.Mode()); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}
