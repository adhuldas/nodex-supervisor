// Package config loads and validates the on-device Nodexa Agent
// configuration from /etc/nodexa/nodexa.conf and /etc/nodexa/config.d/*.conf.
//
// The format is intentionally a small, strict INI-style key=value format
// rather than a general-purpose language. Nodexa Agent is a trusted,
// security-sensitive component: unknown keys are rejected, values are
// type-checked, and nothing in this file is capable of altering the future
// backend trust relationship (pinned backend identity, device certificates).
// That relationship will live in its own protected, non-config-driven store.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Default filesystem locations. These follow the Nodexa OS filesystem
// layout convention and must not be overridden by configuration itself.
const (
	DefaultConfigFile = "/etc/nodexa/nodexa.conf"
	DefaultConfigDir  = "/etc/nodexa/config.d"

	// DefaultRunDir is per-OS (rundir_*.go): macOS has no /run.
	DefaultSocketPath   = DefaultRunDir + "/agent.sock"
	DefaultDataDir      = "/var/lib/nodexa"
	DefaultIdentityDir  = "/var/lib/nodexa/identity"
	DefaultStateDir     = "/var/lib/nodexa/state"
	DefaultContainerDir = "/var/lib/nodexa/containers"
)

// Config holds validated Nodexa Agent runtime configuration.
type Config struct {
	SocketPath      string
	SocketGroup     string
	DataDir         string
	RunDir          string
	LogLevel        string
	IdentityDir     string
	IdentityProvide string // provider override: "auto", "qemu", "linux"
	StateDir        string
	ContainerDir    string
	HealthInterval  time.Duration
	WatchdogEnabled bool
}

// Default returns the built-in configuration used when no config file is
// present (e.g. first boot, or local development).
func Default() *Config {
	return &Config{
		SocketPath:      DefaultSocketPath,
		SocketGroup:     "nodexa",
		DataDir:         DefaultDataDir,
		RunDir:          DefaultRunDir,
		LogLevel:        "info",
		IdentityDir:     DefaultIdentityDir,
		IdentityProvide: "auto",
		StateDir:        DefaultStateDir,
		ContainerDir:    DefaultContainerDir,
		HealthInterval:  30 * time.Second,
		WatchdogEnabled: true,
	}
}

// allowedKeys enumerates every key nodexa.conf is permitted to set. Anything
// else is a hard error: silently ignoring unknown keys in a trusted device
// component is how config drift and confused-deputy bugs get in.
var allowedKeys = map[string]bool{
	"socket_path":      true,
	"socket_group":     true,
	"data_dir":         true,
	"run_dir":          true,
	"log_level":        true,
	"identity_dir":     true,
	"identity_provider": true,
	"state_dir":        true,
	"container_dir":    true,
	"health_interval":  true,
	"watchdog_enabled": true,
}

// Load reads the default config file and config.d drop-ins, layering them
// on top of Default(). A missing config file is not an error: it just means
// defaults are used, which must be a safe, secure posture on its own.
func Load() (*Config, error) {
	return LoadFrom(DefaultConfigFile, DefaultConfigDir)
}

// LoadFrom loads configuration from an explicit file and drop-in directory.
// Exposed separately so it is easy to unit test and to point nodexa-agent at
// alternate paths in QEMU/dev environments.
func LoadFrom(mainFile, dropInDir string) (*Config, error) {
	cfg := Default()

	files := []string{}
	if _, err := os.Stat(mainFile); err == nil {
		files = append(files, mainFile)
	}

	if entries, err := os.ReadDir(dropInDir); err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".conf") {
				names = append(names, e.Name())
			}
		}
		sort.Strings(names)
		for _, n := range names {
			files = append(files, filepath.Join(dropInDir, n))
		}
	}

	for _, f := range files {
		if err := applyFile(cfg, f); err != nil {
			return nil, fmt.Errorf("config: %s: %w", f, err)
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func applyFile(cfg *Config, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			return fmt.Errorf("line %d: expected key=value", lineNo)
		}
		key := strings.TrimSpace(parts[0])
		val := strings.Trim(strings.TrimSpace(parts[1]), `"`)

		if !allowedKeys[key] {
			return fmt.Errorf("line %d: unknown configuration key %q", lineNo, key)
		}

		if err := setField(cfg, key, val); err != nil {
			return fmt.Errorf("line %d: %w", lineNo, err)
		}
	}
	return scanner.Err()
}

func setField(cfg *Config, key, val string) error {
	switch key {
	case "socket_path":
		cfg.SocketPath = val
	case "socket_group":
		cfg.SocketGroup = val
	case "data_dir":
		cfg.DataDir = val
	case "run_dir":
		cfg.RunDir = val
	case "log_level":
		switch val {
		case "debug", "info", "warn", "error":
			cfg.LogLevel = val
		default:
			return fmt.Errorf("invalid log_level %q", val)
		}
	case "identity_dir":
		cfg.IdentityDir = val
	case "identity_provider":
		switch val {
		case "auto", "qemu", "linux":
			cfg.IdentityProvide = val
		default:
			return fmt.Errorf("invalid identity_provider %q", val)
		}
	case "state_dir":
		cfg.StateDir = val
	case "container_dir":
		cfg.ContainerDir = val
	case "health_interval":
		d, err := time.ParseDuration(val)
		if err != nil {
			return fmt.Errorf("invalid health_interval %q: %w", val, err)
		}
		cfg.HealthInterval = d
	case "watchdog_enabled":
		b, err := strconv.ParseBool(val)
		if err != nil {
			return fmt.Errorf("invalid watchdog_enabled %q: %w", val, err)
		}
		cfg.WatchdogEnabled = b
	}
	return nil
}

// Validate ensures the configuration is internally consistent and safe to
// run with. It never touches the filesystem beyond path shape checks.
func (c *Config) Validate() error {
	if !filepath.IsAbs(c.SocketPath) {
		return fmt.Errorf("socket_path must be an absolute path: %q", c.SocketPath)
	}
	if !filepath.IsAbs(c.DataDir) {
		return fmt.Errorf("data_dir must be an absolute path: %q", c.DataDir)
	}
	if !filepath.IsAbs(c.RunDir) {
		return fmt.Errorf("run_dir must be an absolute path: %q", c.RunDir)
	}
	if c.HealthInterval < time.Second {
		return fmt.Errorf("health_interval must be at least 1s, got %s", c.HealthInterval)
	}
	return nil
}
