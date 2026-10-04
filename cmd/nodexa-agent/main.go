// Command nodexa-agent is Nodexa OS's trusted device supervisor: the single
// long-running service responsible for device identity, health monitoring,
// persistent state, and local container management, exposed to nodexactl
// over /run/nodexa/agent.sock.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/api"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/backend"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/config"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/container"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/deploy"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/events"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/gsm"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/health"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/identity"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/location"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/provisioning"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/state"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/storage"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/system"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/update"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/version"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/vpn"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/watchdog"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/wifi"
)

const (
	// runcPath is where the nodexa-container-runtime recipe installs runc.
	runcPath = "/usr/bin/runc"
	// containerSeedDir holds reference bundles baked into the read-only
	// image by the nodexa-test-container recipe.
	containerSeedDir = "/usr/share/nodexa/containers"
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("nodexa-agent: ")

	// The agent is a daemon with no subcommands, so anything on the command
	// line is someone probing it (e.g. `nodexa-agent version` over SSH).
	// Starting up for that used to launch a second full agent outside the
	// service's mount namespace, which fought the real one over container
	// netns mounts and the bridge DNS port (observed 2026-09-24). Handled
	// before OTA delegation so a probe never counts as a boot attempt.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-version", "-v":
			fmt.Printf("nodex-supervisor %s (commit %s, built %s)\n", version.AgentVersion, version.Commit, version.BuildDate)
			return
		default:
			fmt.Fprintf(os.Stderr, "usage: %s [--version]\n", filepath.Base(os.Args[0]))
			os.Exit(2)
		}
	}

	// Small boards (the BeagleBone has 512 MB): collect garbage sooner than
	// Go's default, which lets the heap double between collections. Both
	// stay overridable through GOGC / GOMEMLIMIT in the environment.
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(50)
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(48 << 20) // soft: GC harder near it, never fails
	}

	// If an OTA binary exists and has not crash-looped, delegate execution to it immediately.
	maybeDelegateToOTA(update.DefaultOTABinaryPath, update.DefaultAttemptPath)

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("configuration error: %v", err)
	}

	// Only one agent may manage the device's containers and networks. The
	// lock fd is O_CLOEXEC (Go's default), so an in-place syscall.Exec
	// after an OTA drops it and the new image takes it again.
	lockFile, err := acquireInstanceLock(filepath.Join(cfg.RunDir, "agent.lock"))
	if err != nil {
		log.Fatalf("%v", err)
	}
	defer lockFile.Close()

	bus := events.NewBus(512)
	bus.Emit(events.AgentStarted, "nodexa-agent starting", events.Fieldsf("version", "%s", version.AgentVersion))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- persistent state & boot bookkeeping ---
	stateStore, err := state.Open(cfg.StateDir)
	if err != nil {
		log.Fatalf("state store: %v", err)
	}
	bootRecord, err := stateStore.RecordBoot()
	if err != nil {
		log.Printf("warning: failed to persist boot record: %v", err)
	}
	bus.Emit(events.Boot, "boot recorded", events.Fieldsf("boot_count", "%d", bootRecord.BootCount))

	// --- identity ---
	idProvider := identity.ForName(cfg.IdentityProvide)
	idManager := identity.NewManager(cfg.IdentityDir, idProvider)
	deviceIdentity, err := idManager.Load()
	if err != nil {
		log.Fatalf("identity: %v", err)
	}
	bus.Emit(events.IdentityReady, "device identity ready", events.Fieldsf("device_id", "%s", deviceIdentity.DeviceID))

	// --- provisioning (flash-time config.json seed; see internal/provisioning) ---
	provisioningData, err := provisioning.Load(provisioning.DefaultMountPath)
	if err != nil {
		log.Printf("warning: provisioning: %v", err)
		provisioningData = &provisioning.Data{}
	}
	// --- Wi-Fi (handed to NetworkManager; see internal/wifi) ---
	// Before backend registration, so a Wi-Fi-only device (e.g. a laptop
	// booted from a Nodex Nomad stick) can reach the cloud at all.
	applyWifi(ctx, provisioningData.Wifi, bus)

	// --- backend registration (Phase-1.5 bridge; see internal/backend) ---
	var backendClient *backend.Client
	var backendToken tokenHolder
	registeredCh := make(chan struct{}, 1)
	// A confirmed cloud URL change overrides the flash-time / default URL.
	cloudURLProv := provisioningData.CloudURL
	if (cloudURLProv == nil || *cloudURLProv == "") && version.GetDefaultCloudURL() != "" {
		defaultURL := version.GetDefaultCloudURL()
		cloudURLProv = &defaultURL
	}
	if cloudURL := resolveCloudURL(stateStore, cloudURLProv); cloudURL != nil && *cloudURL != "" {
		backendClient = backend.NewClient(*cloudURL)
		// A confirmed fleet transfer overrides the flash-time fleet.
		fleetID := resolveFleetID(stateStore, provisioningData.FleetID)
		go registerWithBackend(ctx, backendClient, deviceIdentity, fleetID, bus, &backendToken, registeredCh)
	} else {
		// Otherwise the device runs standalone with nothing in the log to
		// say why it never shows up in the cloud.
		log.Println("warning: no cloud URL configured (cloud_url in config.json); device will not register or send heartbeats")
	}

	// --- VPN (ops-only direct SSH access; see internal/vpn) ---
	var vpnIP vpnIPHolder
	tailscaleAuthKey := provisioningData.TailscaleAuthKey
	if (tailscaleAuthKey == nil || *tailscaleAuthKey == "") && version.GetDefaultTailscaleAuthKey() != "" {
		defaultKey := version.GetDefaultTailscaleAuthKey()
		tailscaleAuthKey = &defaultKey
	}
	if tailscaleAuthKey != nil && *tailscaleAuthKey != "" {
		provider := vpn.TailscaleProvider{
			AuthKey:  *tailscaleAuthKey,
			Hostname: deviceIdentity.DeviceID,
		}
		go connectVPN(ctx, provider, bus, &vpnIP)
	}

	// --- container runtime ---
	containerMgr := container.NewManager(cfg.ContainerDir, containerSeedDir, cfg.DataDir+"/image-cache", cfg.DataDir+"/volumes", runcPath, cfg.RunDir+"/runc", bus)
	if container.IsDockerAvailable() {
		containerMgr.SetEngine(container.EngineDocker)
		log.Println("container runtime: using Docker")
	} else if container.IsNerdctlAvailable() {
		containerMgr.SetEngine(container.EngineNerdctl)
		log.Println("container runtime: using nerdctl")
	} else {
		containerMgr.SetEngine(container.EngineRunc)
		log.Println("container runtime: using runc")
	}
	containerMgr.SetDeviceEnv(map[string]string{"DEVICE_ID": deviceIdentity.DeviceID})
	if err := containerMgr.Bootstrap(); err != nil {
		log.Printf("warning: container bootstrap: %v", err)
	}
	if containerMgr.Ready() {
		bus.Emit(events.ContainerRuntimeReady, "container runtime ready", nil)
	}

	networkMgr, err := container.NewNetworkManager(cfg.DataDir+"/networks", bus)
	if err != nil {
		log.Printf("warning: network manager init: %v", err)
	} else {
		containerMgr.SetNetworkManager(networkMgr)
	}

	// Restart containers whose process exited, per their compose restart
	// policy (internal/container/restart.go).
	go containerMgr.Supervise(ctx)

	// Auto-start every seeded reference container (e.g. nodexa-test) so a
	// freshly flashed device shows visible activity even before any Nodexa
	// Deploy deployment is assigned. deploy.Manager.Apply stops these again
	// the moment a real deployment lands (see its own doc comment) -- a
	// seeded container is a demo/validation workload, never meant to
	// compete with a real one indefinitely.
	//
	// Run in its own goroutine, off main()'s startup path: nodexa-agent.service
	// is Type=notify with WatchdogSec=30 (see that unit), so systemd only
	// considers the agent up once watchdog.NotifyReady() below is reached --
	// which only happens after the API server starts listening. A slow or
	// stuck `runc run` here must never block that: it would take
	// /run/nodexa/agent.sock down with it, leaving nodexactl unable to even
	// reach the agent to report what's wrong (exactly the failure mode a
	// demo/seed container should never be able to cause).
	go func() {
		// Only until the first real deployment: after that the seed-retired
		// marker persists on the data partition until a fresh flash.
		if containerMgr.SeedsRetired() {
			return
		}
		seeded, err := containerMgr.SeededNames()
		if err != nil {
			log.Printf("warning: listing seeded containers: %v", err)
			return
		}
		for _, name := range seeded {
			if err := containerMgr.Start(name); err != nil {
				log.Printf("warning: starting seeded container %q: %v", name, err)
			}
		}
	}()

	// --- deploy (Nodexa Deploy poll/apply loop; see internal/deploy) ---
	deployMgr := deploy.NewManager(containerMgr, bus)

	// --- update (Nodexa Agent OTA updater; see internal/update) ---
	agentUpdater := update.NewAgentUpdater("/usr/bin/nodexa-agent", cfg.DataDir+"/bin/nodexa-agent", cfg.RunDir, bus)

	// --- host OS A/B updater; settles an update installed before the last reboot ---
	osUpdater := update.NewOSUpdater(cfg.DataDir, cfg.RunDir, bus)
	if p := osUpdater.Resume(); p != nil {
		log.Printf("host OS update to %s: %s %s", p.Version, p.State, p.Error)
	}

	// --- dashboard-requested device actions (identify, reboot, ...) ---
	transfers := &fleetTransfers{client: backendClient, deviceID: deviceIdentity.DeviceID, store: stateStore, provisioned: provisioningData.FleetID, deployMgr: deployMgr}
	actions := &actionRunner{client: backendClient, deviceID: deviceIdentity.DeviceID, containers: containerMgr, volumesDir: cfg.DataDir + "/volumes", bus: bus}
	cloudURLs := &cloudURLChanges{
		client: backendClient, identity: deviceIdentity, store: stateStore, provisioned: provisioningData.CloudURL,
		fleetID: func() *string { return resolveFleetID(stateStore, provisioningData.FleetID) },
		token:   &backendToken, bus: bus,
	}

	// Device location resolution follows:
	// 1. Hardware GPS (Cellular GNSS via ModemManager / NMEA)
	// 2. Static provisioned location (config.json)
	// 3. IPv6-based geolocation
	// 4. nil (backend places device by incoming request IP)
	var provLocation *backend.Location
	if l := provisioningData.Location; l != nil {
		provLocation = &backend.Location{Latitude: l.Latitude, Longitude: l.Longitude}
	}
	locationResolver := location.NewResolver(location.Config{
		Provisioned: provLocation,
	})

	// --- health ---
	healthChecker := health.NewChecker(cfg.DataDir, containerMgr.Ready, hasNetworkConnectivity)

	// What the disk's used space consists of (Docker, logs, ...), measured
	// in the background and sent with the heartbeat once per scan.
	storageScanner := storage.NewScanner(cfg.DataDir, cfg.DataDir)
	go storageScanner.Run(ctx)

	// --- API server ---
	server := api.New(cfg.SocketPath, api.Deps{
		Identity:    deviceIdentity,
		HealthCheck: healthChecker,
		Containers:  containerMgr,
		Networks:    networkMgr,
		Deploy:      deployMgr,
		Update:      agentUpdater,
		State:       stateStore,
		Events:      bus,
		SocketGroup: cfg.SocketGroup,
		VPNIP:       vpnIP.Get,
	})

	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- server.ListenAndServe()
	}()

	// --- watchdog ---
	wd := watchdog.New()
	go wd.Run(ctx, func() bool {
		// Minimal liveness check: the API socket must still be accept()-able
		// and the container runtime must still be reachable. A future
		// iteration can widen this to a fuller self-test.
		return probeSocket(cfg.SocketPath) && containerMgr.Ready()
	})

	// --- periodic health reporting ---
	containerStateCh := make(chan struct{}, 1)
	containerMgr.SetOnStateChange(func() {
		select {
		case containerStateCh <- struct{}{}:
		default:
		}
	})
	actionDoneCh := make(chan struct{}, 1)
	triggerHeartbeat := func() {
		select {
		case actionDoneCh <- struct{}{}:
		default:
		}
	}
	actions.onDone = triggerHeartbeat
	wifi.OnConnected = func() {
		if !actions.IsRunning() {
			triggerHeartbeat()
		}
	}
	go func() {
		ticker := time.NewTicker(cfg.HealthInterval)
		defer ticker.Stop()
		containerReport := containerReportState{}
		appUsage := appUsageTracker{}

		// doTick runs a single health-report + heartbeat cycle. It is
		// wrapped with panic recovery so a transient crash on one tick
		// cannot permanently kill the health-report goroutine.
		doTick := func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("panic in health report tick (recovered): %v\n%s", r, debug.Stack())
				}
			}()

			if actions.IsRunning() {
				return
			}

			report := healthChecker.Check()
			bus.Emit(events.HealthReport, "periodic health report", map[string]string{
				"overall": string(report.Overall),
				"runtime": string(report.Runtime),
				"network": string(report.Network),
			})
			if backendClient != nil {
				if token := backendToken.Get(); token != "" {
					containers := containerReport.next(containerMgr)
					apps := appUsage.sample(containerMgr, report)
					loc := locationResolver.Resolve(ctx)
					// nmcli/mmcli can stall on D-Bus; this whole loop is one
					// goroutine, so a stuck probe would stop every heartbeat.
					probeCtx, cancelProbe := context.WithTimeout(ctx, 15*time.Second)
					isWifi := wifi.Supported(probeCtx)
					isGSM := gsm.Available(probeCtx)
					var wifiNets []wifi.WifiNetwork
					if isWifi {
						wifiNets, _ = wifi.Scan(probeCtx)
					}
					conns, wifiSSID := wifi.DetectConnections(probeCtx)
					cancelProbe()
					storageReport := storageScanner.Latest()
					resp := sendHeartbeat(ctx, backendClient, deviceIdentity.DeviceID, token, report, containers, apps, storageReport, vpnIP.Get(), loc, isWifi, isGSM, conns, wifiSSID, wifiNets, agentUpdater, osUpdater)
					if resp != nil {
						if resp.FleetTransfer != nil {
							transfers.handle(ctx, token, *resp.FleetTransfer)
						} else if resp.DeploymentRevision == nil && !deployMgr.IsApplying() && deployMgr.HasDeployed() {
							// No release assigned (e.g. moved to a fleet without
							// one): same as nodexa-esp32, whatever a release put
							// here is removed.
							go func() {
								if err := deployMgr.RemoveAll(); err != nil {
									log.Printf("warning: removing deployed containers: %v (will retry)", err)
								}
							}()
						}
						if resp.DeploymentRevision != nil && *resp.DeploymentRevision != deployMgr.AppliedRevision() {
							if !deployMgr.IsApplying() {
								go applyDeployment(ctx, backendClient, deviceIdentity.DeviceID, token, deployMgr, bus)
							}
						}
						// If the cloud has pinned a new agent version, trigger OTA update and live reporting.
						// Never while an OS update is being written: the agent restart would cut it off.
						if resp.AgentUpdate != nil && resp.AgentUpdate.Version != version.AgentVersion {
							if !agentUpdater.IsApplying() && !osUpdater.IsApplying() {
								go applyAgentOTA(ctx, backendClient, deviceIdentity.DeviceID, token, agentUpdater, *resp.AgentUpdate, bus)
							}
						}
						// Likewise for a pinned host OS version (A/B slot update, ends in a reboot).
						if resp.OSUpdate != nil && !agentUpdater.IsApplying() && osUpdater.ShouldApply(*resp.OSUpdate) {
							go applyOSOTA(ctx, backendClient, deviceIdentity.DeviceID, token, osUpdater, *resp.OSUpdate, bus)
						}
						if resp.Action != nil {
							actions.handle(ctx, token, *resp.Action)
						}
						if resp.CloudURL != nil {
							cloudURLs.handle(ctx, *resp.CloudURL)
						}
					}
				}
			}
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-registeredCh:
				// Fire an immediate heartbeat right after backend
				// registration completes so nodexa-backend gets
				// metrics ASAP instead of waiting for the next tick.
				registeredCh = nil // disable this case after first fire
				doTick()
			case <-containerStateCh:
				doTick()
			case <-actionDoneCh:
				doTick()
			case <-ticker.C:
				doTick()
			}
		}
	}()

	watchdog.NotifyReady()
	// Boot succeeded: clear any OTA attempt counter so it won't trigger rollback
	attemptFile := filepath.Join(cfg.RunDir, "ota-boot-attempt")
	if _, err := os.Stat(attemptFile); err == nil {
		_ = os.Remove(attemptFile)
		log.Printf("cleared OTA boot attempt counter; running version %s is stable", version.AgentVersion)
	}
	bus.Emit(events.AgentReady, "nodexa-agent ready", nil)
	log.Printf("ready: device=%s os=%s agent=%s socket=%s", deviceIdentity.DeviceID, osUpdater.CurrentVersion(), version.AgentVersion, cfg.SocketPath)

	// --- shutdown handling ---
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-sigCh:
		log.Printf("received signal %s, shutting down", sig)
		watchdog.NotifyStopping()
		cancel()
		_ = server.Close()
	case err := <-serverErrCh:
		if err != nil {
			log.Fatalf("api server: %v", err)
		}
	}
}

// hasNetworkConnectivity is a minimal, dependency-free network readiness
// check: does at least one non-loopback interface have an assigned address?
// This deliberately does not attempt outbound connectivity (edge devices
// may be legitimately offline / air-gapped before a future Nodexa Cloud
// relationship exists) -- it only reports local link readiness.
func hasNetworkConnectivity() bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		if len(addrs) > 0 {
			return true
		}
	}
	return false
}

// probeSocket does a fast, non-blocking dial to confirm the API socket is
// still accepting connections.
func probeSocket(path string) bool {
	c, err := net.DialTimeout("unix", path, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// tokenHolder guards the backend token issued by registerWithBackend: it's
// written from the registration goroutine and read from the periodic
// health-report ticker goroutine.
type tokenHolder struct {
	mu    sync.Mutex
	token string
}

func (h *tokenHolder) Get() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.token
}

func (h *tokenHolder) Set(token string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.token = token
}

// vpnIPHolder guards the device's tailnet IPv4 address resolved by
// connectVPN: written once from that goroutine after a successful
// "tailscale ip -4" and read from the periodic health-report ticker
// goroutine, same read/write split as tokenHolder above.
type vpnIPHolder struct {
	mu sync.Mutex
	ip string
}

func (h *vpnIPHolder) Get() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.ip
}

func (h *vpnIPHolder) Set(ip string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ip = ip
}

// registerBackoffInitial/Max bound registerWithBackend's retry loop: the
// device's network often isn't fully up yet (DHCP still settling) the
// instant the agent starts, and a real device with no eMMC/reboot cycle
// pending should never be stuck permanently unregistered over one
// early-boot blip.
const (
	registerBackoffInitial = 5 * time.Second
	registerBackoffMax     = 2 * time.Minute
)

// registerWithBackend registers this device with the nodexa-backend named
// in its provisioning config.json (see internal/provisioning and
// internal/backend). This runs in its own goroutine and never blocks
// startup: a device with no cloud URL keeps operating standalone, the same
// philosophy as hasNetworkConnectivity. An unreachable backend is retried
// with exponential backoff (capped) until it succeeds or the agent shuts
// down, mirroring the heartbeat loop's own best-effort-forever behavior.
func registerWithBackend(ctx context.Context, client *backend.Client, id *identity.Identity, fleetID *string, bus *events.Bus, token *tokenHolder, registeredCh chan<- struct{}) {
	req := registerRequest(id, fleetID)

	backoff := registerBackoffInitial
	for {
		resp, err := client.Register(ctx, req)
		if err == nil {
			token.Set(resp.Token)
			bus.Emit(events.DeviceRegistered, "registered with nodexa-backend", events.Fieldsf("device_id", "%s", resp.DeviceID))
			// Signal the health-report goroutine so it fires an
			// immediate heartbeat with fresh metrics right away.
			select {
			case registeredCh <- struct{}{}:
			default:
			}
			return
		}
		bus.Emit(events.DeviceRegisterFailed, "backend registration failed, will retry", events.Fieldsf("error", "%s", err))
		log.Printf("warning: backend registration: %v (retrying in %s)", err, backoff)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > registerBackoffMax {
			backoff = registerBackoffMax
		}
	}
}

// registerRequest describes this device to nodexa-backend's register route.
func registerRequest(id *identity.Identity, fleetID *string) backend.RegisterRequest {
	info := system.Collect()
	provider := id.Provider
	if provider == "linux" || provider == "" {
		provider = "third_party"
	}
	deviceType := "third_party"
	return backend.RegisterRequest{
		DeviceID:            id.DeviceID,
		HardwareFingerprint: id.Fingerprint,
		IdentityProvider:    provider,
		OSVersion:           update.RunningOSVersion(),
		AgentVersion:        version.AgentVersion,
		Architecture:        info.Architecture,
		Hostname:            info.Hostname,
		FleetID:             fleetID,
		DeviceType:          &deviceType,
		Platform:            runtime.GOOS,
	}
}

// connectVPN brings the device onto its ops-access tailnet (see
// internal/vpn), retrying with the same backoff policy and
// runs-forever-in-its-own-goroutine shape as registerWithBackend above --
// network often isn't up yet this early in boot, and a device that never
// gets the support team's tailnet reachable should keep retrying rather
// than giving up, same as backend registration. Once connected, it also
// resolves the device's tailnet IPv4 address and stores it in ipHolder so
// the heartbeat loop can report it to nodexa-backend for the support
// team's direct SSH access.
//
// After the initial connection, it monitors VPN health and reconnects if
// tailscaled drops (e.g. OOM-killed or stopped by systemd due to resource
// pressure from rapid log switching in the UI).
func connectVPN(ctx context.Context, provider vpn.Provider, bus *events.Bus, ipHolder *vpnIPHolder) {
	backoff := registerBackoffInitial
	for {
		if err := provider.Connect(ctx); err == nil {
			bus.Emit(events.VPNConnected, "connected to ops-access tailnet", nil)
			// Resolve the tailnet IPv4 with retries: tailscaled may
			// not have assigned the IP yet immediately after "up"
			// succeeds, so a single attempt can fail transiently.
			ipBackoff := registerBackoffInitial
			resolved := false
			for {
				if ip, err := provider.IP(ctx); err != nil {
					log.Printf("warning: vpn ip: %v (will retry)", err)
				} else {
					ipHolder.Set(ip)
					resolved = true
					break
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(ipBackoff):
				}
				if ipBackoff *= 2; ipBackoff > registerBackoffMax {
					ipBackoff = registerBackoffMax
				}
			}
			if resolved {
				break // move on to the health-monitoring loop
			}
		} else {
			log.Printf("warning: vpn connect failed, will retry: %v", err)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > registerBackoffMax {
			backoff = registerBackoffMax
		}
	}

	// Health-monitoring loop: periodically verify tailscaled is still
	// running and reconnect if it dropped. On third-party devices,
	// tailscaled can be killed by systemd watchdog or OOM when the host
	// is under resource pressure; without this loop the VPN stays down
	// forever after the initial successful connection.
	const vpnHealthInterval = 2 * time.Minute
	ticker := time.NewTicker(vpnHealthInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			st := provider.Status()
			if st.Connected {
				continue
			}
			log.Printf("warning: vpn health check: tailscale disconnected, attempting reconnect")
			bus.Emit(events.VPNConnected, "vpn disconnected, reconnecting", nil)
			if err := provider.Connect(ctx); err != nil {
				log.Printf("warning: vpn reconnect failed: %v (will retry at next health check)", err)
				continue
			}
			if ip, err := provider.IP(ctx); err == nil {
				ipHolder.Set(ip)
			}
			bus.Emit(events.VPNConnected, "vpn reconnected after health check", nil)
			log.Printf("vpn reconnected successfully")
		}
	}
}

// sendHeartbeat reports this device's current versions, latest health
// snapshot, tailnet IP (if any), live OTA update progress, and (per containerReportState) an
// up-to-date container list to nodexa-backend. Failures are logged, not
// fatal -- heartbeats are best-effort, same as registration itself.
// Returns nil on failure, or the response the caller uses to detect a
// deployment revision change or pinned agent/OS update.
func sendHeartbeat(ctx context.Context, client *backend.Client, deviceID, token string, report health.Report, containers *[]backend.ContainerState, apps []backend.AppUsage, storageReport *storage.Breakdown, tailscaleIP string, location *backend.Location, isWifi bool, isGSM bool, conns []string, wifiSSID string, wifiNets []wifi.WifiNetwork, updater *update.AgentUpdater, osUpdater *update.OSUpdater) *backend.HeartbeatResponse {
	var ip *string
	if tailscaleIP != "" {
		ip = &tailscaleIP
	}
	containerCount := "nil (unchanged)"
	if containers != nil {
		containerCount = fmt.Sprintf("%d", len(*containers))
	}
	log.Printf("sending heartbeat: device=%s cpu=%.1f%% mem=%.1f%% disk=%.1f%% containers=%s tailscale_ip=%q",
		deviceID, report.CPUPercent, report.MemUsedPercent, report.DiskUsedPercent, containerCount, tailscaleIP)

	var updateStatus *backend.AgentUpdateProgress
	if updater != nil {
		updateStatus = updater.CurrentProgress()
	}
	osVersion := update.RunningOSVersion()
	var osUpdateStatus *backend.AgentUpdateProgress
	if osUpdater != nil {
		osVersion = osUpdater.CurrentVersion()
		osUpdateStatus = osUpdater.HeartbeatStatus()
	}

	deviceType := "third_party"
	var backendWifiNets []backend.WifiNetwork
	if isWifi {
		for _, wn := range wifiNets {
			backendWifiNets = append(backendWifiNets, backend.WifiNetwork{
				SSID:          wn.SSID,
				SignalPercent: wn.SignalPercent,
				Security:      wn.Security,
			})
		}
	}

	resp, err := client.Heartbeat(ctx, deviceID, token, backend.HeartbeatRequest{
		OSVersion:         osVersion,
		AgentVersion:      version.AgentVersion,
		CPUPercent:        report.CPUPercent,
		MemUsedPercent:    report.MemUsedPercent,
		DiskUsedPercent:   report.DiskUsedPercent,
		TemperatureC:      report.TemperatureC,
		MemTotalBytes:     report.MemTotalBytes,
		MemUsedBytes:      report.MemUsedBytes,
		DiskTotalBytes:    report.DiskTotalBytes,
		DiskUsedBytes:     report.DiskUsedBytes,
		SwapTotalBytes:    report.SwapTotalBytes,
		SwapUsedBytes:     report.SwapUsedBytes,
		StorageBreakdown:  storageReport,
		CPUCores:          runtime.NumCPU(),
		GPUName:           report.GPUName,
		GPUCores:          report.GPUCores,
		GPUPercent:        report.GPUPercent,
		LoadAvg1:          &report.LoadAvg1,
		LoadAvg5:          &report.LoadAvg5,
		LoadAvg15:         &report.LoadAvg15,
		UptimeSeconds:     report.UptimeSeconds,
		Containers:        containers,
		TailscaleIP:       ip,
		AgentUpdateStatus: updateStatus,
		OSUpdateStatus:    osUpdateStatus,
		CloudURL:          client.BaseURL(),
		Location:          location,
		Apps:              apps,
		DeviceType:        &deviceType,
		IsWifi:            &isWifi,
		IsGSM:             &isGSM,
		Connections:       conns,
		WifiSSID:          wifiSSID,
		WifiError:         wifi.LastError(),
		WifiNetworks:      backendWifiNets,
	})
	if err != nil {
		log.Printf("warning: backend heartbeat: %v", err)
		return nil
	}
	if osUpdateStatus != nil && !osUpdater.IsApplying() {
		osUpdater.Reported()
	}
	log.Printf("heartbeat accepted: status=%s last_seen_at=%s", resp.Status, resp.LastSeenAt)
	return resp
}

// containerReportInterval is the hard fallback for containerReportState:
// even an unchanged container list is resent at least this often, so
// nodexa-backend's containers_updated_at never silently drifts arbitrarily
// far behind reality on a device whose containers just don't change for a
// long time.
const containerReportInterval = 15 * time.Minute

// containerReportState tracks what nodexa-backend was last told the local
// container list looks like, so the heartbeat loop can omit it on ticks
// where nothing changed (see backend.HeartbeatRequest.Containers) instead of
// resending it on every single heartbeat. Deliberately lives here rather
// than in internal/backend: it's heartbeat-loop bookkeeping, not part of the
// wire contract itself.
type containerReportState struct {
	lastSentAt   time.Time
	lastSentJSON string
}

// next returns the containers to include in this heartbeat -- nil if the
// local list is unchanged since the last report and containerReportInterval
// hasn't elapsed yet. Comparison is done via JSON encoding rather than
// reflect.DeepEqual to sidestep time.Time's monotonic-reading quirks: these
// CreatedAt values all come from JSON-persisted metadata (see
// container/types.go's metadata), never a fresh time.Now() call, so this is
// exact, not an approximation.
func (s *containerReportState) next(mgr *container.NodexaContainerManager) *[]backend.ContainerState {
	containers, err := mgr.List()
	if err != nil {
		log.Printf("warning: listing containers for heartbeat: %v", err)
		return nil
	}

	reported := make([]backend.ContainerState, len(containers))
	for i, c := range containers {
		var pid *int
		if c.PID != 0 {
			pid = &c.PID
		}
		reported[i] = backend.ContainerState{
			Name:               c.Name,
			Image:              c.Image,
			State:              string(c.State),
			PID:                pid,
			CreatedAt:          c.CreatedAt,
			DeploymentName:     c.DeploymentName,
			DeploymentRevision: c.DeploymentRevision,
		}
	}

	encoded, err := json.Marshal(reported)
	if err != nil {
		log.Printf("warning: encoding containers for heartbeat: %v", err)
		return nil
	}
	asJSON := string(encoded)

	changed := asJSON != s.lastSentJSON
	stale := s.lastSentAt.IsZero() || time.Since(s.lastSentAt) >= containerReportInterval
	if !changed && !stale {
		return nil
	}

	s.lastSentJSON = asJSON
	s.lastSentAt = time.Now()
	return &reported
}

// applyDeployment fetches this device's effective deployment and applies
// it via deployMgr. Runs in its own goroutine off the heartbeat ticker
// (deploy.Manager.Apply already serializes against overlapping calls) so a
// slow image pull never delays the next heartbeat tick.
func applyDeployment(ctx context.Context, client *backend.Client, deviceID, token string, deployMgr *deploy.Manager, bus *events.Bus) {
	dep, err := client.GetDeployment(ctx, deviceID, token)
	if err != nil {
		log.Printf("warning: fetching deployment: %v", err)
		return
	}
	if dep == nil {
		return // no deployment currently assigned to this device
	}
	if err := deployMgr.Apply(dep); err != nil {
		if strings.Contains(err.Error(), "already being applied") {
			return
		}
		bus.Emit(events.DeploymentFailed, "applying deployment failed", events.Fieldsf("error", "%s", err))
		log.Printf("warning: applying deployment: %v", err)
	}
}

// applyAgentOTA downloads, verifies, installs, and restarts into the new pinned agent version,
// reporting live progress updates back to Nodexa Cloud.
func applyAgentOTA(ctx context.Context, client *backend.Client, deviceID, token string, updater *update.AgentUpdater, target backend.AgentUpdateTarget, bus *events.Bus) {
	if client != nil {
		target.URL = client.ResolveURL(target.URL)
	}
	bus.Emit(events.UpdateAvailable, "pinned agent update detected from cloud", events.Fieldsf("version", "%s", target.Version))
	log.Printf("cloud pinned agent update to version %s (current %s, url %s); starting OTA update", target.Version, version.AgentVersion, target.URL)

	// Already installed by an earlier attempt whose restart didn't happen
	// (agents before 0.4.0 restarted a unit name third-party hosts don't
	// have): restart into it instead of downloading it again every tick.
	if otaBinaryVersion(update.DefaultOTABinaryPath) == target.Version {
		log.Printf("agent %s is already installed; restarting into it", target.Version)
		_ = updater.Restart()
		return
	}

	err := updater.ApplyFromURL(ctx, target, func(state string, pct int, err error) {
		log.Printf("OTA update progress: state=%s progress=%d%%", state, pct)
		prog := backend.AgentUpdateProgress{
			Version:  target.Version,
			State:    state,
			Progress: pct,
		}
		if err != nil {
			prog.Error = err.Error()
		}
		if client != nil && token != "" {
			_ = client.ReportUpdateProgress(ctx, deviceID, token, prog)
		}
	})

	if err != nil {
		log.Printf("OTA update failed: %v", err)
		if client != nil && token != "" {
			_ = client.ReportUpdateProgress(ctx, deviceID, token, backend.AgentUpdateProgress{
				Version:  target.Version,
				State:    "failed",
				Progress: 0,
				Error:    err.Error(),
			})
		}
		bus.Emit(events.UpdateAvailable, "agent update failed", events.Fieldsf("error", "%v", err))
		return
	}

	log.Printf("OTA update to version %s completed successfully; restarting agent supervisor", target.Version)
	if client != nil && token != "" {
		_ = client.ReportUpdateProgress(ctx, deviceID, token, backend.AgentUpdateProgress{
			Version:  target.Version,
			State:    "completed",
			Progress: 100,
		})
	}
	_ = updater.Restart()
}

// applyOSOTA installs the pinned host OS version into the inactive A/B root
// slot and reboots into it, reporting live progress to Nodexa Cloud. The
// final outcome (booted, or rolled back by boot.scr) is reported by the
// next boot's heartbeat, see OSUpdater.Resume.
func applyOSOTA(ctx context.Context, client *backend.Client, deviceID, token string, updater *update.OSUpdater, target backend.AgentUpdateTarget, bus *events.Bus) {
	if client != nil {
		target.URL = client.ResolveURL(target.URL)
	}
	bus.Emit(events.UpdateAvailable, "pinned host OS update detected from cloud", events.Fieldsf("version", "%s", target.Version))
	log.Printf("cloud pinned host OS version %s (current %s, slot %s, url %s); starting OS update", target.Version, updater.CurrentVersion(), updater.CurrentSlot(), target.URL)

	err := updater.Apply(ctx, target, func(state string, pct int, err error) {
		log.Printf("OS update progress: state=%s progress=%d%%", state, pct)
		prog := backend.AgentUpdateProgress{Version: target.Version, State: state, Progress: pct}
		if err != nil {
			prog.Error = err.Error()
		}
		if client != nil && token != "" {
			_ = client.ReportOSUpdateProgress(ctx, deviceID, token, prog)
		}
	})
	if err != nil {
		log.Printf("OS update to %s failed: %v", target.Version, err)
		bus.Emit(events.UpdateAvailable, "host OS update failed", events.Fieldsf("error", "%v", err))
		return
	}
	log.Printf("OS update to %s installed; rebooting into the new slot", target.Version)
}

// maybeDelegateToOTA checks if an OTA binary is installed at otaPath.
// If present and executable, it manages the crash-loop boot counter in attemptFile
// and delegates execution to the OTA binary using execOTA.
// If the OTA binary has crashed MaxBootAttempts times, it automatically purges it
// to safely fall back to the base golden image.
func maybeDelegateToOTA(otaPath, attemptFile string) {
	info, err := os.Stat(otaPath)
	if err != nil || info.IsDir() || info.Mode()&0111 == 0 {
		return
	}

	self, err := os.Executable()
	if err == nil {
		if resolved, err := filepath.EvalSymlinks(self); err == nil {
			self = resolved
		}
		if otaResolved, err := filepath.EvalSymlinks(otaPath); err == nil {
			if self == otaResolved {
				return // Already executing the OTA binary!
			}
		} else if self == otaPath {
			return
		}
	}

	// An OS update ships its own agent: an OTA binary from before it that
	// isn't newer would otherwise keep running in its place for good.
	if v := otaBinaryVersion(otaPath); v != "" && !newerVersion(v, version.AgentVersion) {
		log.Printf("OTA binary %s is %s, not newer than built-in %s; removing it", otaPath, v, version.AgentVersion)
		_ = os.Remove(otaPath)
		_ = os.Remove(attemptFile)
		return
	}

	attempts := 0
	if data, err := os.ReadFile(attemptFile); err == nil {
		_, _ = fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &attempts)
	}

	if attempts >= update.MaxBootAttempts {
		log.Printf("warning: OTA binary failed to reach ready state after %d attempts; rolling back to base binary", attempts)
		_ = os.Remove(otaPath)
		_ = os.Remove(attemptFile)
		return
	}

	_ = os.MkdirAll(filepath.Dir(attemptFile), 0755)
	_ = os.WriteFile(attemptFile, []byte(fmt.Sprintf("%d\n", attempts+1)), 0644)

	log.Printf("delegating execution to OTA binary %s (boot attempt %d/%d)...", otaPath, attempts+1, update.MaxBootAttempts)
	if err := execOTA(otaPath, os.Args, os.Environ()); err != nil {
		log.Printf("warning: exec OTA %s failed: %v (rolling back to base)", otaPath, err)
		_ = os.Remove(otaPath)
		_ = os.Remove(attemptFile)
	}
}

// applyWifi hands config.json's Wi-Fi network to NetworkManager. Failure is
// logged, never fatal: wired Ethernet (and a standalone device) still work.
func applyWifi(ctx context.Context, creds *provisioning.WifiCredentials, bus *events.Bus) {
	if creds == nil {
		if err := wifi.Remove(ctx, wifi.DefaultConnectionDir); err != nil {
			log.Printf("warning: %v", err)
		}
		return
	}
	changed, err := wifi.Apply(ctx, wifi.DefaultConnectionDir, wifi.Credentials{SSID: creds.SSID, Password: creds.Password})
	if err != nil {
		log.Printf("warning: provisioned wifi not applied: %v", err)
		return
	}
	if changed {
		bus.Emit(events.NetworkReady, "wifi network configured", events.Fieldsf("ssid", "%s", creds.SSID))
	}
}
