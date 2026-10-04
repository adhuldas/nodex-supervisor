//go:build !windows

package update

import (
	"io"
	"os"
	"path/filepath"
)

// installDir is where updates are staged: beside the OTA binary, so the
// final rename is atomic.
func (u *AgentUpdater) installDir() string {
	return filepath.Dir(u.otaBinary)
}

// install puts the new binary at the OTA path; on the next start
// cmd/nodexa-agent's maybeDelegateToOTA execs it, and falls back to the
// installed binary after MaxBootAttempts boots that never reach ready.
func (u *AgentUpdater) install(staged string) error {
	return os.Rename(staged, u.otaBinary)
}

// installCtl installs nodexactl and configures nodex and nodexa symlinks.
func (u *AgentUpdater) installCtl(staged string) error {
	destDir := u.installDir()
	otaCtl := filepath.Join(destDir, "nodexactl")
	_ = os.Remove(otaCtl)
	if err := os.Rename(staged, otaCtl); err != nil {
		if err := copyExecutable(staged, otaCtl); err != nil {
			return err
		}
	}
	_ = os.Chmod(otaCtl, 0755)

	// Create nodex and nodexa symlinks beside otaCtl in the OTA directory
	otaNodex := filepath.Join(destDir, "nodex")
	_ = os.Remove(otaNodex)
	_ = os.Symlink("nodexactl", otaNodex)

	otaNodexa := filepath.Join(destDir, "nodexa")
	_ = os.Remove(otaNodexa)
	_ = os.Symlink("nodexactl", otaNodexa)

	// Also install or link to system bin paths (/usr/local/bin, /usr/bin) if available & writable
	for _, sysDir := range []string{"/usr/local/bin", "/usr/bin"} {
		if fi, err := os.Stat(sysDir); err == nil && fi.IsDir() {
			sysCtl := filepath.Join(sysDir, "nodexactl")
			if err := copyExecutable(otaCtl, sysCtl); err == nil {
				sysNodexa := filepath.Join(sysDir, "nodexa")
				_ = os.Remove(sysNodexa)
				_ = os.Symlink(sysCtl, sysNodexa)

				sysNodex := filepath.Join(sysDir, "nodex")
				if canOverwriteNodexLink(sysNodex) {
					_ = os.Remove(sysNodex)
					_ = os.Symlink(sysCtl, sysNodex)
				}
			}
		}
	}
	return nil
}

func canOverwriteNodexLink(linkPath string) bool {
	fi, err := os.Lstat(linkPath)
	if os.IsNotExist(err) {
		return true
	}
	if err == nil && (fi.Mode()&os.ModeSymlink != 0) {
		return true
	}
	return false
}

func copyExecutable(src, dest string) error {
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
