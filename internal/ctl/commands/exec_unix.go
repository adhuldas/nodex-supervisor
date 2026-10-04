//go:build !windows

package commands

import (
	"os"
	"os/exec"
	"syscall"
)

// replaceProcess runs args[0] in place of nodexactl, so TTY allocation,
// signals (Ctrl+C, terminal resize) and the exit code are the command's own.
func replaceProcess(args []string) error {
	path, err := exec.LookPath(args[0])
	if err != nil {
		return err
	}
	return syscall.Exec(path, args, os.Environ())
}
