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
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/backend"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/events"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/version"
)

const (
	DefaultBaseBinaryPath = "/usr/bin/nodexa-agent"
	DefaultOTABinaryPath  = "/var/lib/nodexa/bin/nodexa-agent"
	DefaultAttemptPath    = "/run/nodexa/ota-boot-attempt"
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
	destDir := filepath.Dir(u.otaBinary)
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

	// 4. Verify ELF binary format and architecture compatibility
	if err := verifyELF(stagedPath); err != nil {
		report("failed", 100, err)
		return fmt.Errorf("verify elf binary: %w", err)
	}

	// 5. Atomically install
	report("installing", 100, nil)
	if err := os.Chmod(stagedPath, 0755); err != nil {
		report("failed", 100, err)
		return fmt.Errorf("chmod binary: %w", err)
	}

	if err := os.Rename(stagedPath, u.otaBinary); err != nil {
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

	destDir := filepath.Dir(u.otaBinary)
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

	if err := verifyELF(stagedPath); err != nil {
		return fmt.Errorf("verify elf binary: %w", err)
	}

	if err := os.Chmod(stagedPath, 0755); err != nil {
		return fmt.Errorf("chmod binary: %w", err)
	}

	if err := os.Rename(stagedPath, u.otaBinary); err != nil {
		return fmt.Errorf("install ota binary: %w", err)
	}

	attemptFile := filepath.Join(u.bootAttemptDir, "ota-boot-attempt")
	_ = os.Remove(attemptFile)

	if u.bus != nil {
		u.bus.Emit(events.UpdateReady, "nodexa-agent update installed from file", events.Fieldsf("version", "%s", targetVersion))
	}
	return nil
}

// Restart triggers agent restart. If running under systemd, systemd will restart it.
func (u *AgentUpdater) Restart() error {
	// Try systemctl first if present
	if _, err := exec.LookPath("systemctl"); err == nil {
		cmd := exec.Command("systemctl", "restart", "nodexa-agent.service")
		if err := cmd.Start(); err == nil {
			return nil
		}
	}
	// Fall back to clean exit; systemd Restart=always will restart the process
	go func() {
		time.Sleep(200 * time.Millisecond)
		os.Exit(0)
	}()
	return nil
}

// extractOrCopyBinary inspects srcPath: if it is a tar.gz archive, it extracts the
// nodexa-agent binary. If it is already an ELF binary, it copies it directly.
func extractOrCopyBinary(srcPath, destPath string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer f.Close()

	header := make([]byte, 4)
	n, _ := f.Read(header)
	_, _ = f.Seek(0, io.SeekStart)

	if n >= 2 && header[0] == 0x1f && header[1] == 0x8b {
		// Gzip archive (.tar.gz)
		gzReader, err := gzip.NewReader(f)
		if err != nil {
			return fmt.Errorf("open gzip reader: %w", err)
		}
		defer gzReader.Close()

		tarReader := tar.NewReader(gzReader)
		for {
			hdr, err := tarReader.Next()
			if err != nil {
				if err == io.EOF {
					break
				}
				return fmt.Errorf("read tar archive: %w", err)
			}
			base := filepath.Base(hdr.Name)
			if (base == "nodexa-agent" || base == "nodexa-supervisor" || strings.HasPrefix(base, "nodexa-agent-") || strings.HasPrefix(base, "nodexa-supervisor-")) && !hdr.FileInfo().IsDir() {
				out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
				if err != nil {
					return err
				}
				if _, err := io.Copy(out, tarReader); err != nil {
					_ = out.Close()
					return err
				}
				return out.Close()
			}
		}
		return errors.New("tar archive did not contain nodexa-agent or nodexa-supervisor executable")
	}

	if n >= 4 && header[0] == 0x7f && header[1] == 'E' && header[2] == 'L' && header[3] == 'F' {
		// Raw ELF binary
		out, err := os.OpenFile(destPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, f)
		return err
	}

	return errors.New("unrecognized file format: expected ELF binary or tar.gz archive")
}

// verifyELF verifies that the file is a valid executable ELF binary compatible
// with the host architecture.
func verifyELF(binaryPath string) error {
	elffile, err := elf.Open(binaryPath)
	if err != nil {
		return fmt.Errorf("invalid elf binary: %w", err)
	}
	defer elffile.Close()

	if elffile.Type != elf.ET_EXEC && elffile.Type != elf.ET_DYN {
		return fmt.Errorf("elf file is not an executable (type=%s)", elffile.Type)
	}

	// Architecture compatibility check
	switch runtime.GOARCH {
	case "arm":
		if elffile.Machine != elf.EM_ARM {
			return fmt.Errorf("architecture mismatch: device is arm, binary is %s", elffile.Machine)
		}
	case "arm64":
		if elffile.Machine != elf.EM_AARCH64 {
			return fmt.Errorf("architecture mismatch: device is arm64, binary is %s", elffile.Machine)
		}
	case "amd64":
		if elffile.Machine != elf.EM_X86_64 {
			return fmt.Errorf("architecture mismatch: device is amd64, binary is %s", elffile.Machine)
		}
	}

	return nil
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
