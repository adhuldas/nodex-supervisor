//go:build windows

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

func acquireInstanceLock(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("instance lock: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("instance lock: %w", err)
	}
	return f, nil
}

func execOTA(otaPath string, args []string, env []string) error {
	return fmt.Errorf("OTA in-place exec not supported on Windows")
}
