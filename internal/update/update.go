// Package update handles Over-The-Air (OTA) updates for Nodexa OS and
// nodexa-agent.
//
// In Nodexa OS, the root filesystem (/usr) is mounted read-only for system
// integrity. The base golden agent binary resides at /usr/bin/nodexa-agent.
// When an OTA agent update is pinned and applied, it is installed into
// /var/lib/nodexa/bin/nodexa-agent on the persistent read-write partition.
// The supervisor delegates execution to this binary, protected by a
// crash-loop watchdog that automatically rolls back to the base golden
// binary if an update fails to reach readiness.
package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/backend"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/config"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/events"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/version"
)

const (
	DefaultBaseBinaryPath = "/usr/bin/nodexa-agent"
	DefaultOTABinaryPath  = "/var/lib/nodexa/bin/nodexa-agent"
	DefaultAttemptPath    = config.DefaultRunDir + "/ota-boot-attempt"
	MaxBootAttempts       = 3
)

// AgentStatus reports the current update and binary status of nodexa-agent.
type AgentStatus struct {
	CurrentVersion    string `json:"current_version"`
	RunningBinary     string `json:"running_binary"`
	IsOTAActive       bool   `json:"is_ota_active"`
	OTAPresent        bool   `json:"ota_present"`
	RollbackAvailable bool   `json:"rollback_available"`
}

// ProgressFunc is a callback invoked during update progress.
type ProgressFunc func(state string, pct int, err error)

// AgentUpdater manages downloading, verifying, installing, and rolling back
// nodexa-agent binaries.
type AgentUpdater struct {
	mu             sync.Mutex
	baseBinary     string
	otaBinary      string
	bootAttemptDir string
	bus            *events.Bus
	inProgress     bool
	lastProgress   backend.AgentUpdateProgress
}

// NewAgentUpdater creates an AgentUpdater.
func NewAgentUpdater(baseBinary, otaBinary, bootAttemptDir string, bus *events.Bus) *AgentUpdater {
	if baseBinary == "" {
		baseBinary = DefaultBaseBinaryPath
	}
	if otaBinary == "" {
		otaBinary = DefaultOTABinaryPath
	}
	if bootAttemptDir == "" {
		bootAttemptDir = filepath.Dir(DefaultAttemptPath)
	}
	return &AgentUpdater{
		baseBinary:     baseBinary,
		otaBinary:      otaBinary,
		bootAttemptDir: bootAttemptDir,
		bus:            bus,
	}
}

// IsApplying returns whether an OTA update is actively in progress.
func (u *AgentUpdater) IsApplying() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.inProgress
}

// CurrentProgress returns the latest progress snapshot if an update is underway.
func (u *AgentUpdater) CurrentProgress() *backend.AgentUpdateProgress {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.inProgress {
		return nil
	}
	cp := u.lastProgress
	return &cp
}

// Status returns current agent OTA binary information.
func (u *AgentUpdater) Status() AgentStatus {
	u.mu.Lock()
	defer u.mu.Unlock()

	runningBin, err := os.Executable()
	if err != nil {
		runningBin = u.baseBinary
	}
	// Resolve symlinks if any
	if resolved, err := filepath.EvalSymlinks(runningBin); err == nil {
		runningBin = resolved
	}

	otaStat, err := os.Stat(u.otaBinary)
	otaPresent := err == nil && !otaStat.IsDir()

	isOTAActive := strings.HasPrefix(runningBin, filepath.Dir(u.otaBinary)) || runningBin == u.otaBinary

	return AgentStatus{
		CurrentVersion:    version.AgentVersion,
		RunningBinary:     runningBin,
		IsOTAActive:       isOTAActive,
		OTAPresent:        otaPresent,
		RollbackAvailable: otaPresent,
	}
}

// Rollback unlinks the OTA binary so that the supervisor reverts to the base golden image.
func (u *AgentUpdater) Rollback() error {
	u.mu.Lock()
	defer u.mu.Unlock()

	attemptFile := filepath.Join(u.bootAttemptDir, "ota-boot-attempt")
	_ = os.Remove(attemptFile)

	if err := os.Remove(u.otaBinary); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("rollback failed: %w", err)
	}

	if u.bus != nil {
		u.bus.Emit(events.UpdateCompleted, "nodexa-agent rolled back to base binary", events.Fieldsf("base_binary", "%s", u.baseBinary))
	}
	return nil
}

// ApplyFromURL downloads an OTA target from a remote URL, verifies checksum and ELF architecture,
// and installs it to the OTA binary location.
func (u *AgentUpdater) ApplyFromURL(ctx context.Context, target backend.AgentUpdateTarget, progress ProgressFunc) error {
	u.mu.Lock()
	if u.inProgress {
		u.mu.Unlock()
		return errors.New("update already in progress")
	}
	u.inProgress = true
	u.mu.Unlock()

	defer func() {
		u.mu.Lock()
		u.inProgress = false
		u.mu.Unlock()
	}()

	report := func(state string, pct int, err error) {
		u.mu.Lock()
		u.lastProgress = backend.AgentUpdateProgress{
			Version:  target.Version,
			State:    state,
			Progress: pct,
		}
		if err != nil {
			u.lastProgress.Error = err.Error()
		}
		u.mu.Unlock()

		if progress != nil {
			progress(state, pct, err)
		}
	}

	report("downloading", 0, nil)

	if !strings.HasPrefix(target.URL, "http://") && !strings.HasPrefix(target.URL, "https://") {
		err := fmt.Errorf("invalid download URL %q: must start with http:// or https://", target.URL)
		report("failed", 0, err)
		return err
	}

	// Ensure destination directory exists
	destDir := u.installDir()
	if err := os.MkdirAll(destDir, 0755); err != nil {
		report("failed", 0, err)
		return fmt.Errorf("create ota bin dir: %w", err)
	}

	// 1. Download to temporary file
	tmpDownloadPath := filepath.Join(destDir, fmt.Sprintf(".nodexa-agent-download-%d.tmp", time.Now().UnixNano()))
	defer os.Remove(tmpDownloadPath)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, target.URL, nil)
	if err != nil {
		report("failed", 0, err)
		return fmt.Errorf("create download request: %w", err)
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		report("failed", 0, err)
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("download failed with status %d", resp.StatusCode)
		report("failed", 0, err)
		return err
	}

	tmpFile, err := os.OpenFile(tmpDownloadPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		report("failed", 0, err)
		return fmt.Errorf("open temp file: %w", err)
	}

	hasher := sha256.New()
	totalSize := resp.ContentLength
	var downloadedBytes int64
	lastReportedPct := 0

	buf := make([]byte, 32*1024)
	for {
		select {
		case <-ctx.Done():
			_ = tmpFile.Close()
			report("failed", lastReportedPct, ctx.Err())
			return ctx.Err()
		default:
		}

		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := tmpFile.Write(buf[:n]); werr != nil {
				_ = tmpFile.Close()
				report("failed", lastReportedPct, werr)
				return fmt.Errorf("write temp download: %w", werr)
			}
			hasher.Write(buf[:n])
			downloadedBytes += int64(n)

			if totalSize > 0 {
				pct := int((downloadedBytes * 100) / totalSize)
				if pct-lastReportedPct >= 10 || pct == 100 {
					lastReportedPct = pct
					report("downloading", pct, nil)
				}
			}
		}

		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			_ = tmpFile.Close()
			report("failed", lastReportedPct, readErr)
			return fmt.Errorf("read download stream: %w", readErr)
		}
	}
	_ = tmpFile.Close()

	// 2. Checksum verification
	report("verifying", 100, nil)
	actualSHA256 := hex.EncodeToString(hasher.Sum(nil))
	if target.SHA256 != "" {
		if !strings.EqualFold(actualSHA256, target.SHA256) {
			err := fmt.Errorf("sha256 mismatch: expected %s, got %s", target.SHA256, actualSHA256)
			report("failed", 100, err)
			return err
		}
	}

	// 3. Extract or extract binary
	stagedPath := filepath.Join(destDir, fmt.Sprintf(".nodexa-agent-staged-%d.tmp", time.Now().UnixNano()))
	defer os.Remove(stagedPath)

	if err := extractOrCopyBinary(tmpDownloadPath, stagedPath); err != nil {
		report("failed", 100, err)
		return fmt.Errorf("extract binary: %w", err)
	}

	// 4. Verify it's an executable for this host's OS and CPU
	if err := verifyBinary(stagedPath); err != nil {
		report("failed", 100, err)
		return fmt.Errorf("verify binary: %w", err)
	}

	// 5. Atomically install
	report("installing", 100, nil)
	if err := os.Chmod(stagedPath, 0755); err != nil {
		report("failed", 100, err)
		return fmt.Errorf("chmod binary: %w", err)
	}

	if err := u.install(stagedPath); err != nil {
		report("failed", 100, err)
		return fmt.Errorf("atomic install ota binary: %w", err)
	}

	// Reset boot attempt counter
	attemptFile := filepath.Join(u.bootAttemptDir, "ota-boot-attempt")
	_ = os.Remove(attemptFile)

	report("completed", 100, nil)

	if u.bus != nil {
		u.bus.Emit(events.UpdateReady, "nodexa-agent update installed successfully", events.Fieldsf("version", "%s", target.Version))
	}

	return nil
}

// ApplyFromFile installs an OTA binary from a local file path.
func (u *AgentUpdater) ApplyFromFile(ctx context.Context, srcPath, expectedSHA256, targetVersion string) error {
	u.mu.Lock()
	if u.inProgress {
		u.mu.Unlock()
		return errors.New("update already in progress")
	}
	u.inProgress = true
	u.mu.Unlock()

	defer func() {
		u.mu.Lock()
		u.inProgress = false
		u.mu.Unlock()
	}()

	destDir := u.installDir()
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("create ota bin dir: %w", err)
	}

	// Checksum verification if expected
	if expectedSHA256 != "" {
		hasher := sha256.New()
		f, err := os.Open(srcPath)
		if err != nil {
			return fmt.Errorf("open src file: %w", err)
		}
		if _, err := io.Copy(hasher, f); err != nil {
			_ = f.Close()
			return fmt.Errorf("hash src file: %w", err)
		}
		_ = f.Close()

		actual := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(actual, expectedSHA256) {
			return fmt.Errorf("sha256 mismatch: expected %s, got %s", expectedSHA256, actual)
		}
	}

	stagedPath := filepath.Join(destDir, fmt.Sprintf(".nodexa-agent-staged-%d.tmp", time.Now().UnixNano()))
	defer os.Remove(stagedPath)

	if err := extractOrCopyBinary(srcPath, stagedPath); err != nil {
		return fmt.Errorf("extract binary: %w", err)
	}

	if err := verifyBinary(stagedPath); err != nil {
		return fmt.Errorf("verify binary: %w", err)
	}

	if err := os.Chmod(stagedPath, 0755); err != nil {
		return fmt.Errorf("chmod binary: %w", err)
	}

	if err := u.install(stagedPath); err != nil {
		return fmt.Errorf("install ota binary: %w", err)
	}

	attemptFile := filepath.Join(u.bootAttemptDir, "ota-boot-attempt")
	_ = os.Remove(attemptFile)

	if u.bus != nil {
		u.bus.Emit(events.UpdateReady, "nodexa-agent update installed from file", events.Fieldsf("version", "%s", targetVersion))
	}
	return nil
}

// Restart exits so the service manager starts the new binary: systemd
// (Restart=always), launchd (KeepAlive) and the Windows scheduled task's
// wrapper loop all restart on exit, under whatever unit name install.sh
// gave them -- restarting a named unit here only fits one of them.
func (u *AgentUpdater) Restart() error {
	go func() {
		time.Sleep(200 * time.Millisecond)
		os.Exit(0)
	}()
	return nil
}

// extractOrCopyBinary writes the supervisor executable from srcPath to
// destPath: srcPath is a release archive (.tar.gz, or .zip on Windows) or
// the bare executable.
func extractOrCopyBinary(srcPath, destPath string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()

	header := make([]byte, 4)
	n, _ := io.ReadFull(f, header)
	_, _ = f.Seek(0, io.SeekStart)
	header = header[:n]

	switch {
	case len(header) >= 2 && header[0] == 0x1f && header[1] == 0x8b:
		gzReader, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("open gzip reader: %w", err)
		}
		defer gzReader.Close()
		tarReader := tar.NewReader(gzReader)
		for {
			hdr, err := tarReader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("read tar archive: %w", err)
			}
			if !hdr.FileInfo().IsDir() && isSupervisorBinaryName(hdr.Name) {
				return writeExecutable(destPath, tarReader)
			}
		}
		return errors.New("archive did not contain a nodex-supervisor executable")
	case len(header) >= 4 && string(header) == "PK\x03\x04":
		info, err := f.Stat()
		if err != nil {
			return err
		}
		zr, err := zip.NewReader(f, info.Size())
		if err != nil {
			return fmt.Errorf("open zip archive: %w", err)
		}
		for _, zf := range zr.File {
			if zf.FileInfo().IsDir() || !isSupervisorBinaryName(zf.Name) {
				continue
			}
			rc, err := zf.Open()
			if err != nil {
				return err
			}
			defer rc.Close()
			return writeExecutable(destPath, rc)
		}
		return errors.New("archive did not contain a nodex-supervisor executable")
	default:
		// A bare executable; verifyBinary checks what it is.
		return writeExecutable(destPath, f)
	}
}

// isSupervisorBinaryName matches the executable in a release archive:
// nodex-supervisor, or its nodexa-agent alias (".exe" on Windows).
func isSupervisorBinaryName(name string) bool {
	base := strings.TrimSuffix(filepath.Base(filepath.ToSlash(name)), ".exe")
	return base == "nodex-supervisor" || base == "nodexa-agent"
}

func writeExecutable(destPath string, r io.Reader) error {
	out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, r); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// verifyBinary checks that the file is an executable for this host: ELF
// on Linux, Mach-O on macOS, PE on Windows, built for this CPU. A build
// for another OS fails here instead of being installed and never starting.
func verifyBinary(path string) error {
	want := runtime.GOOS + "/" + runtime.GOARCH
	got, err := binaryPlatform(path)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("platform mismatch: device is %s, binary is %s", want, got)
	}
	return nil
}

// binaryPlatform reports the GOOS/GOARCH an executable was built for.
func binaryPlatform(path string) (string, error) {
	if f, err := elf.Open(path); err == nil {
		defer f.Close()
		if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
			return "", fmt.Errorf("elf file is not an executable (type=%s)", f.Type)
		}
		switch f.Machine {
		case elf.EM_X86_64:
			return "linux/amd64", nil
		case elf.EM_AARCH64:
			return "linux/arm64", nil
		case elf.EM_ARM:
			return "linux/arm", nil
		}
		return "", fmt.Errorf("unsupported elf machine %s", f.Machine)
	}
	if f, err := macho.Open(path); err == nil {
		defer f.Close()
		if f.Type != macho.TypeExec {
			return "", fmt.Errorf("mach-o file is not an executable (type=%s)", f.Type)
		}
		switch f.Cpu {
		case macho.CpuAmd64:
			return "darwin/amd64", nil
		case macho.CpuArm64:
			return "darwin/arm64", nil
		}
		return "", fmt.Errorf("unsupported mach-o cpu %s", f.Cpu)
	}
	if f, err := pe.Open(path); err == nil {
		defer f.Close()
		switch f.Machine {
		case pe.IMAGE_FILE_MACHINE_AMD64:
			return "windows/amd64", nil
		case pe.IMAGE_FILE_MACHINE_ARM64:
			return "windows/arm64", nil
		}
		return "", fmt.Errorf("unsupported pe machine %#x", f.Machine)
	}
	return "", errors.New("not an executable (expected ELF, Mach-O or PE)")
}

// Provider is the general OTA interface for Nodexa Update.
type Provider interface {
	Check(ctx context.Context) (*Available, error)
	Apply(ctx context.Context, u Available) error
	Status() Status
}

// Available describes an update offered by Nodexa Cloud.
type Available struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
}

// Status describes the current OTA state.
type Status struct {
	CurrentSlot string `json:"current_slot,omitempty"`
	Pending     bool   `json:"pending"`
}

// Unimplemented is a no-op Provider used until Nodexa Update OS-level A/B exists.
type Unimplemented struct{}

func (Unimplemented) Check(ctx context.Context) (*Available, error) { return nil, nil }
func (Unimplemented) Apply(ctx context.Context, u Available) error  { return nil }
func (Unimplemented) Status() Status                                { return Status{Pending: false} }
