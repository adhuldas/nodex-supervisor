package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/backend"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/container"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/events"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/swap"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/wifi"
)

// identifyDuration is how long the "identify" action blinks the board LEDs.
const identifyDuration = 5 * time.Second

// ledsDir holds the board's sysfs LEDs (the BeagleBone's USR0-3); on a
// machine without any, identify simply has nothing to blink.
const ledsDir = "/sys/class/leds"

// boardLEDDriver is the driver of real, GPIO-wired LEDs. Others under
// ledsDir are left alone: the SD/eMMC controllers' activity LEDs
// (sdhci-omap's "mmc0::", "mmc1::") oops the kernel when given a trigger
// while their controller is powered down -- killing the writing thread,
// and with it identify, before it restores anything.
const boardLEDDriver = "leds-gpio"

// actionRunner executes the dashboard-requested device actions handed to
// the agent in heartbeat responses (see backend.HeartbeatResponse.Action).
// The backend keeps returning the same action until it's reported
// completed or failed, so runner remembers the last ID it picked up and
// never starts the same action twice -- nor two actions at once.
type actionRunner struct {
	client     *backend.Client
	deviceID   string
	containers *container.NodexaContainerManager
	volumesDir string
	bus        *events.Bus
	onDone     func()
	usage      *engineUsageCollector

	mu      sync.Mutex
	lastID  string
	running bool
}

func (r *actionRunner) IsRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// handle starts target in its own goroutine unless it has already been
// picked up, so a slow action never delays the next heartbeat tick.
func (r *actionRunner) handle(ctx context.Context, token string, target backend.DeviceActionTarget) {
	r.mu.Lock()
	if target.ID == r.lastID || r.running {
		r.mu.Unlock()
		return
	}
	r.lastID = target.ID
	r.running = true
	r.mu.Unlock()

	go func() {
		defer func() {
			r.mu.Lock()
			r.running = false
			r.mu.Unlock()
			if r.onDone != nil {
				r.onDone()
			}
		}()
		r.run(ctx, token, target)
	}()
}

func (r *actionRunner) run(ctx context.Context, token string, target backend.DeviceActionTarget) {
	log.Printf("device action %s %s(%s) requested from cloud", target.Action, target.Container, target.ID)
	r.report(ctx, token, target.ID, "running", nil)

	var err error
	if strings.HasSuffix(target.Action, "_container") {
		err = r.checkContainer(target.Container)
	}
	if err == nil {
		err = r.dispatch(ctx, token, target)
	}

	if err != nil {
		log.Printf("device action %s (%s) failed: %v", target.Action, target.ID, err)
		r.report(ctx, token, target.ID, "failed", err)
		return
	}
	// reboot/shutdown already reported completed before going down.
	if target.Action != "reboot" && target.Action != "shutdown" {
		r.report(ctx, token, target.ID, "completed", nil)
	}
}

func (r *actionRunner) dispatch(ctx context.Context, token string, target backend.DeviceActionTarget) error {
	switch target.Action {
	case "identify":
		return identify(ctx)
	case "restart_services":
		return r.restartServices()
	case "purge_data":
		return r.purgeData()
	case "start_container":
		return r.containers.Start(target.Container)
	case "stop_container":
		return r.containers.Stop(target.Container)
	case "restart_container":
		if err := r.containers.Stop(target.Container); err != nil {
			return err
		}
		return r.containers.Start(target.Container)
	case "reboot":
		return r.power(ctx, token, target.ID, "reboot")
	case "shutdown":
		return r.power(ctx, token, target.ID, "poweroff")
	case "change_wifi", "set_wifi":
		ssid := target.SSID
		if ssid == "" {
			ssid = target.Container
		}
		if ssid == "" {
			return errors.New("missing wifi ssid in action target")
		}
		creds := wifi.Credentials{
			SSID:     ssid,
			Password: target.Password,
		}
		if err := wifi.Change(ctx, creds); err != nil {
			return err
		}
		if r.bus != nil {
			r.bus.Emit(events.NetworkReady, "wifi network changed via cloud action", events.Fieldsf("ssid", "%s", creds.SSID))
		}
		return nil
	case "prune_dangling_images":
		freed, err := r.containers.PruneDanglingImages(ctx)
		log.Printf("pruned dangling images, freed %d bytes", freed)
		r.refreshUsage()
		return err
	case "clear_container_logs":
		freed, err := r.containers.ClearLogs(ctx, target.Container)
		log.Printf("cleared container logs, freed %d bytes", freed)
		r.refreshUsage()
		return err
	case "create_swap", "update_swap", "resize_swap":
		return swap.Create(ctx, target.SwapSizeMB)
	case "delete_swap", "remove_swap":
		return swap.Delete(ctx)
	default:
		return fmt.Errorf("unsupported action %q", target.Action)
	}
}

// refreshUsage re-measures right after space was reclaimed, so the next
// heartbeat shows it.
func (r *actionRunner) refreshUsage() {
	if r.usage != nil {
		r.usage.Refresh()
	}
}

// checkContainer rejects a container action naming a container this device
// doesn't have -- Stop, unlike Start, would otherwise quietly succeed.
func (r *actionRunner) checkContainer(name string) error {
	list, err := r.containers.List()
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}
	for _, c := range list {
		if c.Name == name {
			return nil
		}
	}
	return fmt.Errorf("no container named %q on this device", name)
}

func (r *actionRunner) report(ctx context.Context, token, id, state string, err error) {
	rep := backend.DeviceActionReport{ID: id, State: state}
	if err != nil {
		rep.Error = err.Error()
	}

	maxAttempts := 1
	if state == "completed" || state == "failed" {
		maxAttempts = 10
	}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 || state == "completed" || state == "failed" {
			r.client.CloseIdleConnections()
		}
		if rerr := r.client.ReportActionStatus(ctx, r.deviceID, token, rep); rerr != nil {
			log.Printf("warning: reporting device action %s (%s) [attempt %d/%d]: %v", id, state, attempt, maxAttempts, rerr)
			if attempt < maxAttempts {
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Duration(attempt) * time.Second):
				}
			}
		} else {
			log.Printf("device action %s reported as %s", id, state)
			return
		}
	}
}

// power reports the action completed first -- once systemctl succeeds
// there is no agent left to report anything -- then reboots or powers off.
func (r *actionRunner) power(ctx context.Context, token, id, verb string) error {
	r.report(ctx, token, id, "completed", nil)
	if out, err := exec.Command("systemctl", verb).CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl %s: %w: %s", verb, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// restartServices stops and starts every container that is currently
// running, leaving stopped ones alone.
func (r *actionRunner) restartServices() error {
	list, err := r.containers.List()
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}
	var firstErr error
	for _, c := range list {
		if c.State != container.StateRunning {
			continue
		}
		if err := r.containers.Stop(c.Name); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("stopping %q: %w", c.Name, err)
			continue
		}
		if err := r.containers.Start(c.Name); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("starting %q: %w", c.Name, err)
		}
	}
	return firstErr
}

// purgeData empties every container's named volumes. Running containers
// are stopped first so nothing writes into a volume mid-purge, and
// restarted afterwards. The volume directories themselves are kept (only
// their contents go): the bundles' bind mounts point at them.
func (r *actionRunner) purgeData() error {
	list, err := r.containers.List()
	if err != nil {
		return fmt.Errorf("listing containers: %w", err)
	}
	var running []string
	for _, c := range list {
		if c.State == container.StateRunning {
			running = append(running, c.Name)
			_ = r.containers.Stop(c.Name)
		}
	}

	firstErr := emptyVolumes(r.volumesDir)

	for _, name := range running {
		if err := r.containers.Start(name); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("restarting %q: %w", name, err)
		}
	}
	return firstErr
}

// emptyVolumes removes everything inside each <volumesDir>/<container>/<volume>.
func emptyVolumes(volumesDir string) error {
	owners, err := os.ReadDir(volumesDir)
	if os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	for _, owner := range owners {
		ownerDir := filepath.Join(volumesDir, owner.Name())
		vols, err := os.ReadDir(ownerDir)
		if err != nil {
			continue
		}
		for _, vol := range vols {
			volDir := filepath.Join(ownerDir, vol.Name())
			if !vol.IsDir() {
				if err := os.RemoveAll(volDir); err != nil {
					return err
				}
				continue
			}
			entries, err := os.ReadDir(volDir)
			if err != nil {
				return err
			}
			for _, e := range entries {
				if err := os.RemoveAll(filepath.Join(volDir, e.Name())); err != nil {
					return fmt.Errorf("purging %s: %w", volDir, err)
				}
			}
		}
	}
	return nil
}

// identify blinks every board LED for identifyDuration, then restores
// each LED's previous trigger.
func identify(ctx context.Context) error {
	leds, err := os.ReadDir(ledsDir)
	if err != nil || len(leds) == 0 {
		return fmt.Errorf("no LEDs found under %s", ledsDir)
	}

	saved := map[string]string{}
	for _, led := range leds {
		dir := filepath.Join(ledsDir, led.Name())
		if driver, err := filepath.EvalSymlinks(filepath.Join(dir, "device", "driver")); err != nil ||
			filepath.Base(driver) != boardLEDDriver {
			continue
		}
		trigger, err := currentTrigger(dir)
		if err != nil {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, "trigger"), []byte("timer"), 0o644); err != nil {
			continue
		}
		_ = os.WriteFile(filepath.Join(dir, "delay_on"), []byte("100"), 0o644)
		_ = os.WriteFile(filepath.Join(dir, "delay_off"), []byte("100"), 0o644)
		saved[dir] = trigger
	}
	if len(saved) == 0 {
		return fmt.Errorf("could not drive any LED under %s", ledsDir)
	}

	select {
	case <-ctx.Done():
	case <-time.After(identifyDuration):
	}

	for dir, trigger := range saved {
		_ = os.WriteFile(filepath.Join(dir, "trigger"), []byte(trigger), 0o644)
	}
	return nil
}

// currentTrigger returns the bracketed entry of an LED's trigger file,
// e.g. "heartbeat" from "none timer [heartbeat] mmc0".
func currentTrigger(ledDir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(ledDir, "trigger"))
	if err != nil {
		return "", err
	}
	for _, f := range strings.Fields(string(b)) {
		if strings.HasPrefix(f, "[") && strings.HasSuffix(f, "]") {
			return strings.Trim(f, "[]"), nil
		}
	}
	return "none", nil
}
