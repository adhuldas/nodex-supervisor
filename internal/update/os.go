package update

// Host OS (A/B) updates.
//
// Disk layout (layers/meta-nodexa/wic/nodexa-beaglebone.wks.in, and
// nodexa-generic-x86-64.wks.in with the same partition numbers): p1 is the
// vfat boot partition, p2/p3 are root slots a/b. The bootloader -- U-Boot's
// boot.scr (nodexa-boot-script) on the BeagleBone, GRUB's grub.cfg
// (nodexa-grub-ab.cfg) on x86-64 -- boots the slot named in p1's
// nodexa.env and passes it to the kernel as nodexa.slot=a|b.
//
// An update is the OS release's own .wic image (zipped, gzipped or raw),
// the same file operators already upload for flashing. OSUpdater streams
// that image's partition 2 into the inactive slot, checks the result,
// points nodexa.env at it with nodexa_upgrade=1 and reboots. boot.scr then
// gives the new slot 3 boots; nodexa-os-commit (an OS service, not the
// agent) clears nodexa_upgrade once it boots healthy, otherwise boot.scr
// falls back to the old slot. On the next start the agent compares the
// running slot with the one it installed to report the outcome.

import (
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/backend"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/events"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/version"
)

const (
	bootEnvName     = "nodexa.env"
	osStateFileName = "os-update.json"
	// osImagePrefix names a downloaded, checksummed image kept until it's
	// installed: <workDir>/os-update-<sha256>.img.
	osImagePrefix = "os-update-"
	sectorSize    = 512

	// GRUB can only write back to nodexa.env (boot counting, rollback) if
	// it's a GRUB environment block: this header, name=value lines, then
	// '#' padding to exactly grubEnvSize bytes.
	grubEnvHeader = "# GRUB Environment Block\n"
	grubEnvSize   = 1024

	// osRetryBackoff keeps a failing OS update (bad URL, unsupported disk
	// layout, ...) from re-downloading hundreds of MB on every heartbeat.
	osRetryBackoff = 15 * time.Minute
)

// ErrNoABLayout means the running system wasn't booted by the A/B boot
// script, i.e. the device was flashed before A/B support and needs one
// reflash with a newer image before it can take OS updates.
var ErrNoABLayout = errors.New("device was not booted from an A/B root slot; reflash it with an A/B-capable Nodexa OS image once to enable OS updates")

// osUpdateState is persisted across the reboot into the new slot.
type osUpdateState struct {
	Version     string    `json:"version"`
	FromVersion string    `json:"from_version"`
	FromSlot    string    `json:"from_slot"`
	TargetSlot  string    `json:"target_slot"`
	InstalledAt time.Time `json:"installed_at"`
	// Result is empty until the first start after the reboot, then
	// "completed" or "rolled_back".
	Result string `json:"result,omitempty"`
}

// OSUpdater installs host OS updates into the inactive A/B root slot.
type OSUpdater struct {
	mu           sync.Mutex
	bus          *events.Bus
	stateFile    string
	workDir      string
	mountDir     string
	inProgress   bool
	lastProgress backend.AgentUpdateProgress
	// report is an outcome from a previous boot not yet sent to the cloud.
	report *backend.AgentUpdateProgress

	failedVersion string
	failedAt      time.Time
	// skippedVersion is a pinned version older than the running OS, already
	// reported as skipped.
	skippedVersion string

	// Overridable for tests.
	procDir       string
	sysDir        string
	devDir        string
	osReleasePath string
	bootDir       string
	mount         func(source, target, fstype string, readOnly bool) error
	unmount       func(target string) error
	reboot        func() error
}

// NewOSUpdater creates an OSUpdater keeping its state and download under
// dataDir and its temporary mount points under runDir.
func NewOSUpdater(dataDir, runDir string, bus *events.Bus) *OSUpdater {
	return &OSUpdater{
		bus:           bus,
		stateFile:     filepath.Join(dataDir, osStateFileName),
		workDir:       dataDir,
		mountDir:      filepath.Join(runDir, "os-update"),
		procDir:       "/proc",
		sysDir:        "/sys",
		devDir:        "/dev",
		osReleasePath: "/etc/os-release",
		bootDir:       "/boot",
		mount:         mountFS,
		unmount:       unmountFS,
		reboot: func() error {
			return exec.Command("systemctl", "reboot").Start()
		},
	}
}

// CurrentVersion is the running host OS version: VERSION_ID from
// /etc/os-release. Not version.OSVersion, which is whatever the running
// agent binary was built against and goes stale after an agent OTA.
func (u *OSUpdater) CurrentVersion() string {
	if v := readOSReleaseVersion(u.osReleasePath); v != "" {
		return v
	}
	return RunningOSVersion()
}

// CurrentSlot is the root slot this boot came from ("a"/"b"), or "" when
// the device wasn't booted by the A/B boot script.
func (u *OSUpdater) CurrentSlot() string {
	data, err := os.ReadFile(filepath.Join(u.procDir, "cmdline"))
	if err != nil {
		return ""
	}
	return parseSlot(string(data))
}

// IsApplying reports whether an OS update is being installed right now.
func (u *OSUpdater) IsApplying() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.inProgress
}

// HeartbeatStatus returns the progress to attach to the next heartbeat:
// the live progress while installing, else a not-yet-reported outcome of
// the previous boot's update, else nil.
func (u *OSUpdater) HeartbeatStatus() *backend.AgentUpdateProgress {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.inProgress {
		cp := u.lastProgress
		return &cp
	}
	if u.report != nil {
		cp := *u.report
		return &cp
	}
	return nil
}

// Reported drops the previous boot's outcome once the cloud has it.
func (u *OSUpdater) Reported() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.report = nil
}

// Resume settles an update installed before the last reboot: if this boot
// came up on the slot it installed, it completed; otherwise boot.scr fell
// back to the old slot. Call once at startup.
func (u *OSUpdater) Resume() *backend.AgentUpdateProgress {
	st, err := u.loadState()
	if err != nil || st == nil || st.Result == "rolled_back" {
		return nil
	}
	slot := u.CurrentSlot()
	// "completed" is recorded as soon as the agent starts on the new slot,
	// which is before nodexa-os-commit marks it good: a crash in between
	// still ends in boot.scr rolling back, reported here on the old slot.
	if st.Result == "completed" && slot == st.TargetSlot {
		return nil
	}
	prog := backend.AgentUpdateProgress{Version: st.Version, Progress: 100}
	if slot == st.TargetSlot {
		st.Result = "completed"
		prog.State = "completed"
		u.emit(events.UpdateCompleted, "host OS update booted", st.Version)
	} else {
		st.Result = "rolled_back"
		prog.State = "failed"
		prog.Error = fmt.Sprintf("OS %s did not boot cleanly on slot %s; rolled back to slot %s (OS %s)", st.Version, st.TargetSlot, slot, u.CurrentVersion())
		u.emit(events.UpdateCompleted, "host OS update rolled back", st.Version)
	}
	if err := u.saveState(st); err != nil {
		return nil
	}
	u.mu.Lock()
	u.report = &prog
	u.mu.Unlock()
	return &prog
}

// ShouldApply reports whether target should be installed now: it differs
// from the running OS and isn't older than it, isn't already being
// installed, isn't a version that already rolled back, and didn't just
// fail. An older target is never installed: the next heartbeat reports it
// "skipped".
func (u *OSUpdater) ShouldApply(target backend.AgentUpdateTarget) bool {
	current := u.CurrentVersion()
	if target.Version == "" || target.Version == current {
		return false
	}
	if olderVersion(target.Version, current) {
		u.mu.Lock()
		if u.skippedVersion != target.Version {
			u.skippedVersion = target.Version
			u.report = &backend.AgentUpdateProgress{
				Version:  target.Version,
				State:    "skipped",
				Progress: 100,
				Error:    fmt.Sprintf("already running OS %s, newer than the pinned %s", current, target.Version),
			}
		}
		u.mu.Unlock()
		return false
	}
	u.mu.Lock()
	busy := u.inProgress
	recentFailure := u.failedVersion == target.Version && time.Since(u.failedAt) < osRetryBackoff
	u.mu.Unlock()
	if busy || recentFailure {
		return false
	}
	if st, err := u.loadState(); err == nil && st != nil && st.Version == target.Version && st.Result == "rolled_back" {
		return false
	}
	return true
}

// olderVersion reports whether version a is older than b, comparing
// dot-separated numbers ("0.1.9" < "0.1.10"). Anything else isn't older.
func olderVersion(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		na, nb := 0, 0
		var err error
		if i < len(pa) {
			if na, err = strconv.Atoi(pa[i]); err != nil || na < 0 {
				return false
			}
		}
		if i < len(pb) {
			if nb, err = strconv.Atoi(pb[i]); err != nil || nb < 0 {
				return false
			}
		}
		if na != nb {
			return na < nb
		}
	}
	return false
}

// Apply downloads target, installs its root partition into the inactive
// slot, switches the boot slot and reboots. It only returns on failure
// (or when the reboot itself couldn't be started).
func (u *OSUpdater) Apply(ctx context.Context, target backend.AgentUpdateTarget, progress ProgressFunc) (err error) {
	u.mu.Lock()
	if u.inProgress {
		u.mu.Unlock()
		return errors.New("OS update already in progress")
	}
	u.inProgress = true
	u.mu.Unlock()

	report := func(state string, pct int, err error) {
		u.mu.Lock()
		u.lastProgress = backend.AgentUpdateProgress{Version: target.Version, State: state, Progress: pct}
		if err != nil {
			u.lastProgress.Error = err.Error()
		}
		u.mu.Unlock()
		if progress != nil {
			progress(state, pct, err)
		}
	}
	defer func() {
		u.mu.Lock()
		u.inProgress = false
		if err != nil {
			u.failedVersion = target.Version
			u.failedAt = time.Now()
		}
		u.mu.Unlock()
		if err != nil {
			report("failed", 0, err)
		}
	}()

	report("downloading", 0, nil)

	cur := u.CurrentSlot()
	if cur == "" {
		return ErrNoABLayout
	}
	next := otherSlot(cur)
	disk, rootNum, err := u.rootDisk()
	if err != nil {
		return err
	}
	if rootNum != slotPartition(cur) {
		return fmt.Errorf("running root is partition %d, but boot slot %s should be partition %d", rootNum, cur, slotPartition(cur))
	}
	targetName := partitionName(disk, slotPartition(next))
	targetDev := filepath.Join(u.devDir, targetName)
	targetSize, err := u.partitionBytes(targetName)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(target.URL, "http://") && !strings.HasPrefix(target.URL, "https://") {
		return fmt.Errorf("invalid download URL %q: must start with http:// or https://", target.URL)
	}

	// 1. Download and checksum.
	if err := os.MkdirAll(u.workDir, 0o755); err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}
	download, err := u.fetchImage(ctx, target, func(pct int) { report("downloading", pct, nil) })
	if err != nil {
		return err
	}
	report("verifying", 100, nil)

	// 2. Stream the image's root partition into the inactive slot.
	report("installing", 0, nil)
	written, err := installRootPartition(download, targetDev, targetSize, func(pct int) { report("installing", pct, nil) })
	if err != nil {
		return err
	}

	// 3. Read it back and check it's really the requested release.
	report("verifying", 100, nil)
	if err := verifyWritten(targetDev, written); err != nil {
		return err
	}
	if err := u.checkSlotContents(targetDev, target.Version); err != nil {
		return err
	}

	// 4. Switch slots. The state file goes first so the next start can
	// always tell what happened.
	st := &osUpdateState{
		Version:     target.Version,
		FromVersion: u.CurrentVersion(),
		FromSlot:    cur,
		TargetSlot:  next,
		InstalledAt: time.Now().UTC(),
	}
	if err := u.saveState(st); err != nil {
		return fmt.Errorf("save OS update state: %w", err)
	}
	if err := u.writeBootEnv(filepath.Join(u.devDir, partitionName(disk, 1)), next); err != nil {
		_ = os.Remove(u.stateFile)
		return fmt.Errorf("switch boot slot: %w", err)
	}

	// Installed and verified: the image is no longer needed.
	_ = os.Remove(download)

	report("rebooting", 100, nil)
	u.emit(events.UpdateReady, "host OS update installed, rebooting", target.Version)
	if err := u.reboot(); err != nil {
		return fmt.Errorf("reboot: %w", err)
	}
	return nil
}

// fetchImage returns the path of target's downloaded image. A checksummed
// image is kept under workDir until it has been installed, so a retry
// after a failed install (or an agent restart) reuses it instead of
// pulling hundreds of MB again. Images for any other version are removed.
func (u *OSUpdater) fetchImage(ctx context.Context, target backend.AgentUpdateTarget, progress func(pct int)) (string, error) {
	tmp := filepath.Join(u.workDir, ".os-update-download.tmp")
	_ = os.Remove(tmp)
	if target.SHA256 == "" {
		// Nothing to check a cached copy against: always download afresh.
		if _, err := downloadFile(ctx, target.URL, tmp, progress); err != nil {
			_ = os.Remove(tmp)
			return "", err
		}
		return tmp, nil
	}
	if b, err := hex.DecodeString(target.SHA256); err != nil || len(b) != sha256.Size {
		return "", fmt.Errorf("invalid sha256 %q", target.SHA256)
	}

	cached := filepath.Join(u.workDir, osImagePrefix+strings.ToLower(target.SHA256)+".img")
	old, _ := filepath.Glob(filepath.Join(u.workDir, osImagePrefix+"*.img"))
	for _, p := range old {
		if p != cached {
			_ = os.Remove(p)
		}
	}
	if sum, err := fileSHA256(cached); err == nil {
		if strings.EqualFold(sum, target.SHA256) {
			log.Printf("host OS %s already downloaded, reusing %s", target.Version, cached)
			progress(100)
			return cached, nil
		}
		_ = os.Remove(cached)
	}

	sum, err := downloadFile(ctx, target.URL, tmp, progress)
	if err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	if !strings.EqualFold(sum, target.SHA256) {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("sha256 mismatch: expected %s, got %s", target.SHA256, sum)
	}
	if err := os.Rename(tmp, cached); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("keep downloaded image: %w", err)
	}
	return cached, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (u *OSUpdater) emit(kind, msg, ver string) {
	if u.bus != nil {
		u.bus.Emit(kind, msg, events.Fieldsf("version", "%s", ver))
	}
}

func (u *OSUpdater) loadState() (*osUpdateState, error) {
	data, err := os.ReadFile(u.stateFile)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st osUpdateState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func (u *OSUpdater) saveState(st *osUpdateState) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileSync(u.stateFile, data)
}

// rootDisk returns the disk holding / and /'s partition number on it.
func (u *OSUpdater) rootDisk() (disk string, partNum int, err error) {
	majmin, err := rootMajMin(filepath.Join(u.procDir, "self", "mountinfo"))
	if err != nil {
		return "", 0, err
	}
	link, err := filepath.EvalSymlinks(filepath.Join(u.sysDir, "dev", "block", majmin))
	if err != nil {
		return "", 0, fmt.Errorf("resolve root device %s: %w", majmin, err)
	}
	numData, err := os.ReadFile(filepath.Join(link, "partition"))
	if err != nil {
		return "", 0, fmt.Errorf("root device %s is not a partition: %w", majmin, err)
	}
	partNum, err = strconv.Atoi(strings.TrimSpace(string(numData)))
	if err != nil {
		return "", 0, fmt.Errorf("root partition number: %w", err)
	}
	return filepath.Base(filepath.Dir(link)), partNum, nil
}

func (u *OSUpdater) partitionBytes(name string) (int64, error) {
	data, err := os.ReadFile(filepath.Join(u.sysDir, "class", "block", name, "size"))
	if err != nil {
		return 0, fmt.Errorf("partition %s not found (disk layout predates A/B updates?): %w", name, err)
	}
	sectors, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("partition %s size: %w", name, err)
	}
	return sectors * sectorSize, nil
}

// checkSlotContents mounts the freshly written slot read-only and makes
// sure it is the requested release and can finish an A/B update itself.
// A version mismatch would otherwise reboot into an OS that never matches
// the pin, and the device would reinstall it forever.
func (u *OSUpdater) checkSlotContents(dev, wantVersion string) error {
	dir := filepath.Join(u.mountDir, "root")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := u.mount(dev, dir, "ext4", true); err != nil {
		return fmt.Errorf("mount new root: %w", err)
	}
	defer u.unmount(dir)

	got := readOSReleaseVersion(filepath.Join(dir, "etc", "os-release"))
	if got != wantVersion {
		return fmt.Errorf("image is Nodexa OS %q, not the pinned %q", got, wantVersion)
	}
	kernel := kernelName(filepath.Join(dir, "boot"))
	if kernel == "" {
		return fmt.Errorf("image is not A/B-capable (missing /boot/%s)", strings.Join(kernelNames, " or /boot/"))
	}
	// A BeagleBone image on an x86-64 stick (or the other way round) would
	// never boot: both slots' bootloader expects its own kernel format.
	if running := kernelName(u.bootDir); running != "" && running != kernel {
		return fmt.Errorf("image boots /boot/%s but this device boots /boot/%s: it was built for a different device type", kernel, running)
	}
	if _, err := os.Stat(filepath.Join(dir, "usr/sbin/nodexa-os-commit")); err != nil {
		return errors.New("image is not A/B-capable (missing /usr/sbin/nodexa-os-commit)")
	}
	return nil
}

// kernelNames are the kernels a root slot's /boot can hold: U-Boot boots
// zImage (BeagleBone), GRUB bzImage (x86-64).
var kernelNames = []string{"zImage", "bzImage"}

func kernelName(bootDir string) string {
	for _, n := range kernelNames {
		if _, err := os.Stat(filepath.Join(bootDir, n)); err == nil {
			return n
		}
	}
	return ""
}

// writeBootEnv points the bootloader at slot as an untested update,
// keeping nodexa.env in the GRUB environment block format if that's what
// the boot partition has.
func (u *OSUpdater) writeBootEnv(bootDev, slot string) error {
	dir := filepath.Join(u.mountDir, "boot")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := u.mount(bootDev, dir, "vfat", false); err != nil {
		return fmt.Errorf("mount boot partition: %w", err)
	}
	defer u.unmount(dir)

	data := []byte(formatBootEnv(slot, true))
	old, err := os.ReadFile(filepath.Join(dir, bootEnvName))
	if (err == nil && bytes.HasPrefix(old, []byte(grubEnvHeader))) || isGrubBoot(dir) {
		data = grubEnvBlock(data)
	}
	tmp := filepath.Join(dir, bootEnvName+".tmp")
	if err := writeFileSync(tmp, data); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, bootEnvName))
}

func isGrubBoot(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "EFI", "BOOT", "grub.cfg")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, "EFI")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(dir, "boot", "grub")); err == nil {
		return true
	}
	return runtime.GOARCH == "amd64" || runtime.GOARCH == "386"
}

func formatBootEnv(slot string, upgrade bool) string {
	up := "0"
	if upgrade {
		up = "1"
	}
	return fmt.Sprintf("nodexa_slot=%s\nnodexa_upgrade=%s\nnodexa_tries=0\n", slot, up)
}

func grubEnvBlock(vars []byte) []byte {
	b := append([]byte(grubEnvHeader), vars...)
	return append(b, bytes.Repeat([]byte{'#'}, grubEnvSize-len(b))...)
}

func parseSlot(cmdline string) string {
	for _, f := range strings.Fields(cmdline) {
		if v, ok := strings.CutPrefix(f, "nodexa.slot="); ok && (v == "a" || v == "b") {
			return v
		}
	}
	return ""
}

func otherSlot(s string) string {
	if s == "a" {
		return "b"
	}
	return "a"
}

func slotPartition(s string) int {
	if s == "b" {
		return 3
	}
	return 2
}

// partitionName follows the kernel's naming: mmcblk1 -> mmcblk1p2, sda -> sda2.
func partitionName(disk string, n int) string {
	if disk != "" && disk[len(disk)-1] >= '0' && disk[len(disk)-1] <= '9' {
		return fmt.Sprintf("%sp%d", disk, n)
	}
	return fmt.Sprintf("%s%d", disk, n)
}

// rootMajMin returns the major:minor of the filesystem mounted at / (the
// last such mountinfo entry, i.e. the one on top).
func rootMajMin(mountinfo string) (string, error) {
	f, err := os.Open(mountinfo)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var majmin string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) > 4 && fields[4] == "/" {
			majmin = fields[2]
		}
	}
	if majmin == "" {
		return "", errors.New("root mount not found in mountinfo")
	}
	return majmin, sc.Err()
}

func readOSReleaseVersion(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "VERSION_ID="); ok {
			return strings.Trim(v, `"'`)
		}
	}
	return ""
}

func writeFileSync(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// downloadFile saves url to path and returns its sha256.
func downloadFile(ctx context.Context, url, path string, progress func(pct int)) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("create download request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("download failed with status %d", resp.StatusCode)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("open download file: %w", err)
	}
	defer f.Close()
	h := sha256.New()
	pw := &progressWriter{total: resp.ContentLength, report: progress}
	if _, err := io.Copy(io.MultiWriter(f, h, pw), resp.Body); err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("download: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// progressWriter reports every 10% of total bytes written through it.
type progressWriter struct {
	total   int64
	written int64
	last    int
	report  func(pct int)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.written += int64(len(b))
	if p.total > 0 && p.report != nil {
		pct := int(p.written * 100 / p.total)
		if pct-p.last >= 10 || (pct == 100 && p.last != 100) {
			p.last = pct
			p.report(pct)
		}
	}
	return len(b), nil
}

// installRootPartition copies partition 2 of the disk image in path onto
// dev and returns the sha256/length of what it wrote.
func installRootPartition(path, dev string, devSize int64, progress func(pct int)) (writtenInfo, error) {
	img, closeImg, err := openDiskImage(path)
	if err != nil {
		return writtenInfo{}, err
	}
	defer closeImg()

	br := bufio.NewReaderSize(img, 1<<20)
	mbr := make([]byte, sectorSize)
	if _, err := io.ReadFull(br, mbr); err != nil {
		return writtenInfo{}, fmt.Errorf("read image partition table: %w", err)
	}
	start, size, err := mbrPartition(mbr, 2)
	if err != nil {
		return writtenInfo{}, err
	}
	if size > devSize {
		return writtenInfo{}, fmt.Errorf("image root partition (%d MiB) is larger than slot (%d MiB)", size>>20, devSize>>20)
	}
	if _, err := io.CopyN(io.Discard, br, start-sectorSize); err != nil {
		return writtenInfo{}, fmt.Errorf("seek to image root partition: %w", err)
	}

	out, err := os.OpenFile(dev, os.O_WRONLY, 0)
	if err != nil {
		return writtenInfo{}, fmt.Errorf("open %s: %w", dev, err)
	}
	h := sha256.New()
	pw := &progressWriter{total: size, report: progress}
	if _, err := io.CopyN(io.MultiWriter(out, h, pw), br, size); err != nil {
		out.Close()
		return writtenInfo{}, fmt.Errorf("write %s: %w", dev, err)
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return writtenInfo{}, fmt.Errorf("sync %s: %w", dev, err)
	}
	if err := out.Close(); err != nil {
		return writtenInfo{}, err
	}
	return writtenInfo{size: size, sha256: h.Sum(nil)}, nil
}

type writtenInfo struct {
	size   int64
	sha256 []byte
}

func verifyWritten(dev string, w writtenInfo) error {
	f, err := os.Open(dev)
	if err != nil {
		return fmt.Errorf("open %s: %w", dev, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyN(h, f, w.size); err != nil {
		return fmt.Errorf("read back %s: %w", dev, err)
	}
	if !bytes.Equal(h.Sum(nil), w.sha256) {
		return fmt.Errorf("read-back of %s does not match what was written", dev)
	}
	return nil
}

// openDiskImage returns a stream of the raw disk image stored in path,
// which may be a zip holding one .wic/.img, a gzip, or the raw image.
func openDiskImage(path string) (io.Reader, func(), error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	magic := make([]byte, 4)
	n, _ := io.ReadFull(f, magic)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		f.Close()
		return nil, nil, err
	}

	switch {
	case n >= 4 && bytes.Equal(magic, []byte("PK\x03\x04")):
		f.Close()
		zr, err := zip.OpenReader(path)
		if err != nil {
			return nil, nil, fmt.Errorf("open zip: %w", err)
		}
		var entry *zip.File
		for _, zf := range zr.File {
			base := filepath.Base(zf.Name)
			if zf.FileInfo().IsDir() || strings.HasPrefix(zf.Name, "__MACOSX/") || strings.HasPrefix(base, ".") {
				continue
			}
			if entry != nil {
				zr.Close()
				return nil, nil, errors.New("zip holds more than one file; expected a single disk image")
			}
			entry = zf
		}
		if entry == nil {
			zr.Close()
			return nil, nil, errors.New("zip holds no disk image")
		}
		rc, err := entry.Open()
		if err != nil {
			zr.Close()
			return nil, nil, fmt.Errorf("open %s in zip: %w", entry.Name, err)
		}
		return rc, func() { rc.Close(); zr.Close() }, nil
	case n >= 2 && magic[0] == 0x1f && magic[1] == 0x8b:
		gz, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			return nil, nil, fmt.Errorf("open gzip: %w", err)
		}
		return gz, func() { gz.Close(); f.Close() }, nil
	default:
		return f, func() { f.Close() }, nil
	}
}

// mbrPartition returns the byte offset and length of primary partition n
// (1-4) from a 512-byte MBR.
func mbrPartition(mbr []byte, n int) (int64, int64, error) {
	if len(mbr) < sectorSize || mbr[510] != 0x55 || mbr[511] != 0xaa {
		return 0, 0, errors.New("not an MBR disk image")
	}
	e := mbr[446+(n-1)*16 : 446+n*16]
	if e[4] == 0 {
		return 0, 0, fmt.Errorf("disk image has no partition %d", n)
	}
	start := int64(binary.LittleEndian.Uint32(e[8:12])) * sectorSize
	size := int64(binary.LittleEndian.Uint32(e[12:16])) * sectorSize
	if start < sectorSize || size == 0 {
		return 0, 0, fmt.Errorf("disk image partition %d is empty", n)
	}
	return start, size, nil
}

// RunningOSVersion is OSUpdater.CurrentVersion without an updater.
func RunningOSVersion() string {
	if v := readOSReleaseVersion("/etc/os-release"); v != "" {
		return v
	}
	if v := hostOSVersion(); v != "" {
		return v
	}
	return version.OSVersion
}

func hostOSVersion() string {
	switch runtime.GOOS {
	case "darwin":
		if out, err := exec.Command("sysctl", "-n", "kern.osproductversion").Output(); err == nil {
			if v := strings.TrimSpace(string(out)); v != "" {
				return v
			}
		}
		if out, err := exec.Command("sw_vers", "-productVersion").Output(); err == nil {
			if v := strings.TrimSpace(string(out)); v != "" {
				return v
			}
		}
	case "windows":
		if out, err := exec.Command("powershell", "-NoProfile", "-Command", "[System.Environment]::OSVersion.Version.ToString()").Output(); err == nil {
			if v := strings.TrimSpace(string(out)); v != "" {
				return v
			}
		}
	}
	return ""
}
