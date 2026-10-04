package commands

import (
	"context"
	"fmt"
	"os"
	"os/exec"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/ctl/client"
)

// ExecOptions holds parameters for `nodexactl exec`.
type ExecOptions struct {
	Container   string
	Command     []string
	Interactive bool
	TTY         bool
	User        string
	Workdir     string
	Detach      bool
}

// ContainerExec implements `nodexactl exec [-it] <container> [command...]`.
func ContainerExec(ctx context.Context, c *client.Client, opts ExecOptions) error {
	if opts.Container == "" {
		return fmt.Errorf("container name is required")
	}

	// Default to /bin/sh if no command is specified
	if len(opts.Command) == 0 {
		opts.Command = []string{"/bin/sh"}
	}

	// Verify with nodexa-agent that the container exists and is running
	ctrs, err := c.Containers(ctx)
	if err != nil {
		return fmt.Errorf("connecting to agent: %w", err)
	}

	var target *client.ContainerResponse
	for _, ctr := range ctrs {
		if ctr.Name == opts.Container {
			cCopy := ctr
			target = &cCopy
			break
		}
	}

	if target == nil {
		return fmt.Errorf("container %q not found", opts.Container)
	}
	if target.State != "running" {
		return fmt.Errorf("container %q is not running (state: %s)", opts.Container, target.State)
	}

	// nodex-supervisor hosts run containers under Docker or nerdctl, whose
	// own exec handles TTYs and signals; runc below is the nodexOS path.
	if cli := engineFor(opts.Container); cli != "" {
		args := []string{cli, "exec"}
		if opts.Interactive {
			args = append(args, "-i")
		}
		if opts.TTY {
			if fi, err := os.Stdin.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) != 0 {
				args = append(args, "-t")
			}
		}
		if opts.Detach {
			args = append(args, "-d")
		}
		if opts.User != "" {
			args = append(args, "-u", opts.User)
		}
		if opts.Workdir != "" {
			args = append(args, "-w", opts.Workdir)
		}
		args = append(args, opts.Container)
		args = append(args, opts.Command...)
		return replaceProcess(args)
	}

	runcPath, err := exec.LookPath("runc")
	if err != nil {
		runcPath = "/usr/bin/runc"
	}

	runcRoot := os.Getenv("NODEXA_RUNC_ROOT")
	if runcRoot == "" {
		runcRoot = "/run/nodexa/runc"
	}

	runcArgs := []string{"runc", "--root", runcRoot, "exec"}
	// Pass -t if TTY is requested and standard input is a terminal device
	if opts.TTY {
		if fi, err := os.Stdin.Stat(); err == nil && (fi.Mode()&os.ModeCharDevice) != 0 {
			runcArgs = append(runcArgs, "-t")
		}
	}
	// Note: runc exec does not have an "-i" flag; stdin is attached by default.
	// We accept "-i" and "-it" in nodexactl for full Docker CLI compatibility.
	if opts.Detach {
		runcArgs = append(runcArgs, "-d")
	}
	if opts.User != "" {
		runcArgs = append(runcArgs, "-u", opts.User)
	}
	if opts.Workdir != "" {
		runcArgs = append(runcArgs, "--cwd", opts.Workdir)
	}
	runcArgs = append(runcArgs, opts.Container)
	runcArgs = append(runcArgs, opts.Command...)

	// If runc is available locally on the system, syscall.Exec replaces the
	// nodexactl process with runc. This ensures native TTY allocation, signal
	// propagation (Ctrl+C, Ctrl+D, SIGWINCH terminal resize), and exit code handling.
	if _, err := os.Stat(runcPath); err == nil {
		runcArgs[0] = runcPath
		return replaceProcess(runcArgs)
	}

	// Fallback to agent HTTP API execution if runc binary is not directly executable
	resp, err := c.ContainerExec(ctx, opts.Container, client.ContainerExecRequest{
		Command: opts.Command,
		User:    opts.User,
		Workdir: opts.Workdir,
	})
	if err != nil {
		return err
	}
	if resp.Stdout != "" {
		os.Stdout.WriteString(resp.Stdout)
	}
	if resp.Stderr != "" {
		os.Stderr.WriteString(resp.Stderr)
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("command exited with code %d", resp.ExitCode)
	}
	return nil
}

// engineFor returns the Docker-compatible CLI (docker, then nerdctl) that
// knows container name, or "" when neither does.
func engineFor(name string) string {
	for _, cli := range []string{"docker", "nerdctl"} {
		path, err := exec.LookPath(cli)
		if err != nil {
			continue
		}
		if exec.Command(path, "inspect", "--type", "container", name).Run() == nil {
			return path
		}
	}
	return ""
}
