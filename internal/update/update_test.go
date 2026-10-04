package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/backend"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/events"
)

// buildTestELF creates a tiny valid Linux ELF binary for testing
// buildTestELF builds a trivial executable for the host running the test,
// which is what an update must be to pass verifyBinary.
func buildTestELF(t *testing.T, outPath string) {
	t.Helper()
	buildTestBinary(t, outPath, runtime.GOOS, runtime.GOARCH)
}

func buildTestBinary(t *testing.T, outPath, goos, goarch string) {
	t.Helper()
	srcDir := t.TempDir()
	mainGo := filepath.Join(srcDir, "main.go")
	err := os.WriteFile(mainGo, []byte("package main\nfunc main() {}\n"), 0644)
	if err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	cmd := exec.Command("go", "build", "-o", outPath, mainGo)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build test binary (%s/%s): %v\n%s", goos, goarch, err, string(out))
	}
}

// Every release target is recognised, so a host is never offered a build
// for another OS (same CPU) that installs and then never starts.
func TestBinaryPlatformRecognisesEveryReleaseTarget(t *testing.T) {
	for _, target := range []string{"linux/amd64", "linux/arm64", "linux/arm", "darwin/amd64", "darwin/arm64", "windows/amd64", "windows/arm64"} {
		goos, goarch, _ := strings.Cut(target, "/")
		bin := filepath.Join(t.TempDir(), "nodex-supervisor")
		buildTestBinary(t, bin, goos, goarch)
		got, err := binaryPlatform(bin)
		if err != nil || got != target {
			t.Errorf("binaryPlatform(%s build) = %q, %v", target, got, err)
		}
	}

	other := "windows"
	if runtime.GOOS == "windows" {
		other = "linux"
	}
	bin := filepath.Join(t.TempDir(), "nodex-supervisor")
	buildTestBinary(t, bin, other, runtime.GOARCH)
	if err := verifyBinary(bin); err == nil || !strings.Contains(err.Error(), "platform mismatch") {
		t.Errorf("verifyBinary accepted a %s/%s build on %s/%s: %v", other, runtime.GOARCH, runtime.GOOS, runtime.GOARCH, err)
	}
}

func TestSupervisorBinaryNames(t *testing.T) {
	for name, want := range map[string]bool{
		"./nodex-supervisor": true, "nodex-supervisor.exe": true, "nodexa-agent": true,
		"nodexa-agent.exe": true, "checksums.txt": false, "nodex-supervisor.previous": false,
	} {
		if got := isSupervisorBinaryName(name); got != want {
			t.Errorf("isSupervisorBinaryName(%q) = %v, want %v", name, got, want)
		}
	}
}

// makeTarGz creates a .tar.gz archive containing the file at binaryPath named "nodexa-agent"
func makeTarGz(t *testing.T, binaryPath string) []byte {
	t.Helper()
	binBytes, err := os.ReadFile(binaryPath)
	if err != nil {
		t.Fatalf("read binary: %v", err)
	}

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	hdr := &tar.Header{
		Name: "nodexa-agent",
		Mode: 0755,
		Size: int64(len(binBytes)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write(binBytes); err != nil {
		t.Fatalf("write tar body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}

	return buf.Bytes()
}

func TestAgentUpdaterStatusAndRollback(t *testing.T) {
	tmpDir := t.TempDir()
	baseBin := filepath.Join(tmpDir, "base-agent")
	otaBin := filepath.Join(tmpDir, "ota-agent")
	bootDir := tmpDir

	bus := events.NewBus(10)
	updater := NewAgentUpdater(baseBin, otaBin, bootDir, bus)

	status := updater.Status()
	if status.OTAPresent {
		t.Fatal("expected OTAPresent=false initially")
	}

	// Create dummy OTA binary
	if err := os.WriteFile(otaBin, []byte("fake"), 0755); err != nil {
		t.Fatalf("write fake ota: %v", err)
	}

	status = updater.Status()
	if !status.OTAPresent {
		t.Fatal("expected OTAPresent=true after creating ota binary")
	}

	// Rollback
	if err := updater.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	status = updater.Status()
	if status.OTAPresent {
		t.Fatal("expected OTAPresent=false after rollback")
	}
}

func TestAgentUpdaterApplyFromURLTarGz(t *testing.T) {
	tmpDir := t.TempDir()
	baseBin := filepath.Join(tmpDir, "base-agent")
	otaBin := filepath.Join(tmpDir, "ota", "nodexa-agent")
	bootDir := tmpDir

	testELF := filepath.Join(tmpDir, "test-elf")
	buildTestELF(t, testELF)
	tarGzBytes := makeTarGz(t, testELF)

	hasher := sha256.New()
	hasher.Write(tarGzBytes)
	expectedSHA256 := hex.EncodeToString(hasher.Sum(nil))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(tarGzBytes)
	}))
	defer srv.Close()

	bus := events.NewBus(10)
	updater := NewAgentUpdater(baseBin, otaBin, bootDir, bus)

	var reportedStates []string
	var lastPct int
	err := updater.ApplyFromURL(context.Background(), backend.AgentUpdateTarget{
		Version: "0.2.0",
		URL:     srv.URL + "/update.tar.gz",
		SHA256:  expectedSHA256,
	}, func(state string, pct int, err error) {
		reportedStates = append(reportedStates, state)
		lastPct = pct
	})

	if err != nil {
		t.Fatalf("ApplyFromURL: %v", err)
	}

	if lastPct != 100 {
		t.Fatalf("expected final progress 100, got %d", lastPct)
	}

	foundCompleted := false
	for _, s := range reportedStates {
		if s == "completed" {
			foundCompleted = true
			break
		}
	}
	if !foundCompleted {
		t.Fatalf("expected completed state in %v", reportedStates)
	}

	// Check installed binary exists and is executable
	info, err := os.Stat(otaBin)
	if err != nil {
		t.Fatalf("ota binary stat: %v", err)
	}
	if info.Mode()&0111 == 0 {
		t.Fatalf("ota binary is not executable: mode=%o", info.Mode())
	}
}

func TestAgentUpdaterApplyFromURLChecksumMismatch(t *testing.T) {
	tmpDir := t.TempDir()
	baseBin := filepath.Join(tmpDir, "base-agent")
	otaBin := filepath.Join(tmpDir, "ota", "nodexa-agent")
	bootDir := tmpDir

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("some random data"))
	}))
	defer srv.Close()

	updater := NewAgentUpdater(baseBin, otaBin, bootDir, events.NewBus(10))
	err := updater.ApplyFromURL(context.Background(), backend.AgentUpdateTarget{
		Version: "0.2.0",
		URL:     srv.URL,
		SHA256:  "deadbeef1234567890",
	}, nil)

	if err == nil {
		t.Fatal("expected error on sha256 mismatch")
	}

	if _, err := os.Stat(otaBin); !os.IsNotExist(err) {
		t.Fatal("expected ota binary not to be created on error")
	}
}

func TestAgentUpdaterExtractsAndInstallsCtl(t *testing.T) {
	tmpDir := t.TempDir()
	binDir := filepath.Join(tmpDir, "src-bins")
	_ = os.MkdirAll(binDir, 0755)

	agentBin := filepath.Join(binDir, "nodex-supervisor")
	buildTestELF(t, agentBin)
	agentBytes, err := os.ReadFile(agentBin)
	if err != nil {
		t.Fatalf("read agent binary: %v", err)
	}

	ctlBin := filepath.Join(binDir, "nodexactl")
	buildTestELF(t, ctlBin)
	ctlBytes, err := os.ReadFile(ctlBin)
	if err != nil {
		t.Fatalf("read ctl binary: %v", err)
	}

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	// Add nodex-supervisor
	if err := tw.WriteHeader(&tar.Header{
		Name: "nodex-supervisor",
		Mode: 0755,
		Size: int64(len(agentBytes)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(agentBytes); err != nil {
		t.Fatal(err)
	}

	// Add nodexactl
	if err := tw.WriteHeader(&tar.Header{
		Name: "nodexactl",
		Mode: 0755,
		Size: int64(len(ctlBytes)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(ctlBytes); err != nil {
		t.Fatal(err)
	}

	_ = tw.Close()
	_ = gw.Close()
	tarBytes := buf.Bytes()

	hasher := sha256.New()
	hasher.Write(tarBytes)
	checksum := hex.EncodeToString(hasher.Sum(nil))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(tarBytes)
	}))
	defer srv.Close()

	baseBin := filepath.Join(tmpDir, "base-agent")
	otaDir := filepath.Join(tmpDir, "ota-bin")
	otaBin := filepath.Join(otaDir, "nodexa-agent")

	updater := NewAgentUpdater(baseBin, otaBin, tmpDir, events.NewBus(10))
	err = updater.ApplyFromURL(context.Background(), backend.AgentUpdateTarget{
		Version: "0.4.5",
		URL:     srv.URL,
		SHA256:  checksum,
	}, nil)

	if err != nil {
		t.Fatalf("ApplyFromURL error: %v", err)
	}

	// Verify nodexa-agent was installed
	if _, err := os.Stat(otaBin); err != nil {
		t.Fatalf("expected otaBin to exist: %v", err)
	}

	// Verify nodexactl was installed beside it
	installedCtl := filepath.Join(otaDir, "nodexactl")
	if _, err := os.Stat(installedCtl); err != nil {
		t.Fatalf("expected installedCtl %s to exist: %v", installedCtl, err)
	}

	// Verify nodex and nodexa symlinks exist
	for _, alias := range []string{"nodex", "nodexa"} {
		aliasPath := filepath.Join(otaDir, alias)
		if _, err := os.Lstat(aliasPath); err != nil {
			t.Fatalf("expected alias %s to exist: %v", aliasPath, err)
		}
	}
}

