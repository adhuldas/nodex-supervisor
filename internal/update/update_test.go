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
	"testing"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/backend"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/events"
)

// buildTestELF creates a tiny valid Linux ELF binary for testing
func buildTestELF(t *testing.T, outPath string) {
	t.Helper()
	srcDir := t.TempDir()
	mainGo := filepath.Join(srcDir, "main.go")
	err := os.WriteFile(mainGo, []byte("package main\nfunc main() {}\n"), 0644)
	if err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	targetArch := runtime.GOARCH
	if targetArch != "arm64" && targetArch != "amd64" && targetArch != "arm" {
		targetArch = "arm64"
	}

	cmd := exec.Command("go", "build", "-o", outPath, mainGo)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+targetArch)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build test elf (%s): %v\n%s", targetArch, err, string(out))
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
