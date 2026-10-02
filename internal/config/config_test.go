package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("default config should be valid: %v", err)
	}
}

func TestLoadFromMissingFilesUsesDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadFrom(filepath.Join(dir, "nonexistent.conf"), filepath.Join(dir, "config.d"))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.SocketPath != DefaultSocketPath {
		t.Fatalf("expected default socket path, got %s", cfg.SocketPath)
	}
}

func TestLoadFromAppliesOverrides(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "nodexa.conf")
	os.WriteFile(main, []byte("log_level=debug\nhealth_interval=10s\n"), 0o644)

	cfg, err := LoadFrom(main, filepath.Join(dir, "config.d"))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.LogLevel != "debug" {
		t.Fatalf("expected log_level debug, got %s", cfg.LogLevel)
	}
	if cfg.HealthInterval != 10*time.Second {
		t.Fatalf("expected health_interval 10s, got %s", cfg.HealthInterval)
	}
}

func TestLoadFromRejectsUnknownKey(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "nodexa.conf")
	os.WriteFile(main, []byte("totally_bogus_key=1\n"), 0o644)

	if _, err := LoadFrom(main, filepath.Join(dir, "config.d")); err == nil {
		t.Fatal("expected error for unknown configuration key")
	}
}

func TestConfigDDropInsApplyInSortedOrder(t *testing.T) {
	dir := t.TempDir()
	dropDir := filepath.Join(dir, "config.d")
	os.MkdirAll(dropDir, 0o755)
	os.WriteFile(filepath.Join(dropDir, "10-first.conf"), []byte("log_level=warn\n"), 0o644)
	os.WriteFile(filepath.Join(dropDir, "20-second.conf"), []byte("log_level=error\n"), 0o644)

	cfg, err := LoadFrom(filepath.Join(dir, "nodexa.conf"), dropDir)
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	if cfg.LogLevel != "error" {
		t.Fatalf("expected last drop-in (20-second) to win, got %s", cfg.LogLevel)
	}
}

func TestValidateRejectsRelativePaths(t *testing.T) {
	cfg := Default()
	cfg.SocketPath = "relative/path.sock"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for relative socket_path")
	}
}
