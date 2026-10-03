package gsm

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestAvailableAndConnected_EmptySysfs(t *testing.T) {
	tempDir := t.TempDir()
	sysNetDir = filepath.Join(tempDir, "net")
	sysWwanDir = filepath.Join(tempDir, "wwan")
	mmcliPath = "/nonexistent/mmcli"
	nmcliPath = "/nonexistent/nmcli"

	_ = os.MkdirAll(sysNetDir, 0o755)
	_ = os.MkdirAll(sysWwanDir, 0o755)

	ctx := context.Background()
	if Available(ctx) {
		t.Fatalf("expected Available=false on empty sysfs")
	}
	if Connected(ctx) {
		t.Fatalf("expected Connected=false on empty sysfs")
	}
}

func TestAvailable_WWANInterfacePresent(t *testing.T) {
	tempDir := t.TempDir()
	sysNetDir = filepath.Join(tempDir, "net")
	sysWwanDir = filepath.Join(tempDir, "wwan")
	mmcliPath = "/nonexistent/mmcli"
	nmcliPath = "/nonexistent/nmcli"

	wwan0 := filepath.Join(sysNetDir, "wwan0")
	_ = os.MkdirAll(wwan0, 0o755)
	_ = os.WriteFile(filepath.Join(wwan0, "operstate"), []byte("up\n"), 0o644)

	ctx := context.Background()
	if !Available(ctx) {
		t.Fatalf("expected Available=true when wwan0 interface present")
	}
	if !Connected(ctx) {
		t.Fatalf("expected Connected=true when wwan0 operstate is up")
	}
}
