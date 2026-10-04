package commands

import (
	"os"
	"os/exec"
)

// replaceProcess runs args[0] with nodexactl's stdio; Windows can't exec
// in place.
func replaceProcess(args []string) error {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}
