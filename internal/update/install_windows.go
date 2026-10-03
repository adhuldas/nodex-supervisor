package update

import (
	"fmt"
	"os"
	"path/filepath"
)

// Windows can't exec into the OTA binary (cmd/nodexa-agent's execOTA), so
// updates replace the running exe in place, keeping the old one beside it.

func (u *AgentUpdater) installDir() string {
	if exe, err := runningExe(); err == nil {
		return filepath.Dir(exe)
	}
	return filepath.Dir(u.otaBinary)
}

// install swaps the running exe for staged. Windows refuses to overwrite
// or delete a running exe but does allow renaming it, so it moves aside
// to .previous first.
func (u *AgentUpdater) install(staged string) error {
	exe, err := runningExe()
	if err != nil {
		return err
	}
	previous := exe + ".previous"
	_ = os.Remove(previous)
	if err := os.Rename(exe, previous); err != nil {
		return fmt.Errorf("move running exe aside: %w", err)
	}
	if err := os.Rename(staged, exe); err != nil {
		_ = os.Rename(previous, exe)
		return err
	}
	return nil
}

func runningExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe, nil
}
