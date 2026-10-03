//go:build !windows

package update

import (
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
