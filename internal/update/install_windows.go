package update

import (
	"fmt"
	"io"
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

// installCtl installs nodexactl.exe and creates nodex.exe and nodexa.exe aliases.
func (u *AgentUpdater) installCtl(staged string) error {
	destDir := u.installDir()
	ctlExe := filepath.Join(destDir, "nodexactl.exe")
	previous := ctlExe + ".previous"
	_ = os.Remove(previous)
	_ = os.Rename(ctlExe, previous)
	if err := os.Rename(staged, ctlExe); err != nil {
		_ = os.Rename(previous, ctlExe)
		return err
	}

	// Also make nodex.exe and nodexa.exe copies
	_ = copyFile(ctlExe, filepath.Join(destDir, "nodex.exe"))
	_ = copyFile(ctlExe, filepath.Join(destDir, "nodexa.exe"))
	return nil
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	_ = os.Remove(dest)
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
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
