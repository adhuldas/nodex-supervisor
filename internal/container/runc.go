package container

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// runTimeout bounds how long a single `runc run -d` invocation may take to
// detach. It should return almost immediately (fork the container process,
// then exit) -- if it hasn't returned within this window, something is
// wrong (e.g. a naming collision with an orphaned container from a
// previous boot/restart) and continuing to wait would hold
// NodexaContainerManager's lock forever, taking every other container
// operation -- including the heartbeat loop's container report -- down
// with it.
const runTimeout = 60 * time.Second

// runcState mirrors the subset of `runc state <id>` JSON output Nodexa
// Agent cares about.
type runcState struct {
	ID     string `json:"id"`
	Pid    int    `json:"pid"`
	Status string `json:"status"` // "created", "running", "stopped"
	Bundle string `json:"bundle"`
}

// runner executes runc. It is an interface (rather than free functions)
// purely so tests can substitute a fake implementation without invoking a
// real container runtime.
type runner interface {
	Run(bundle, id string) error
	Kill(id, signal string) error
	Delete(id string) error
	State(id string) (*runcState, error)
	Exec(id string, cmd []string, user, cwd string) (stdout, stderr string, exitCode int, err error)
}

// execRunner shells out to the real runc binary.
type execRunner struct {
	runcPath string
	root     string // runc --root: where runc keeps its own container state
}

func newExecRunner(runcPath, root string) *execRunner {
	return &execRunner{runcPath: runcPath, root: root}
}

func (r *execRunner) command(args ...string) *exec.Cmd {
	full := append([]string{"--root", r.root}, args...)
	cmd := exec.Command(r.runcPath, full...)
	cmd.Env = runcEnv(os.Environ())
	return cmd
}

func (r *execRunner) commandContext(ctx context.Context, args ...string) *exec.Cmd {
	full := append([]string{"--root", r.root}, args...)
	cmd := exec.CommandContext(ctx, r.runcPath, full...)
	cmd.Env = runcEnv(os.Environ())
	return cmd
}

// runcEnv returns env without NOTIFY_SOCKET. The agent runs as a
// Type=notify systemd service, and runc treats an inherited NOTIFY_SOCKET
// as a request to proxy it into the container: `runc run -d` then blocks
// until the workload itself sends READY=1. gunicorn does, nginx never does,
// so nginx's start hit runTimeout while the container was actually up
// (observed 2026-09-24), and systemd logged the workload's forwarded
// notifications as coming from a non-main PID.
func runcEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if strings.HasPrefix(kv, "NOTIFY_SOCKET=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Run starts a container detached (equivalent to `runc run -d -b bundle id`),
// which both creates and starts it in one step.
//
// The container's own stdout/stderr are redirected to <bundle>/container.log
// rather than discarded: runc forks the container process while this
// runc invocation is still alive, so file descriptors handed to it here are
// inherited by the container and remain valid after `runc run -d` itself
// exits. This is what backs the future "container logs" feature and lets
// tests confirm a container is actually doing something, not just "running"
// according to runc's state machine.
func (r *execRunner) Run(bundle, id string) error {
	logPath := filepath.Join(bundle, "container.log")
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("runc run: opening container log: %w", err)
	}
	defer logFile.Close()

	// Kill and force-delete any stale or lingering container process with this ID before running.
	_ = r.Kill(id, "KILL")
	_ = r.Delete(id)

	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	cmd := r.commandContext(ctx, "run", "-d", "-b", bundle, id)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("runc run: timed out after %s waiting to detach (likely a naming collision with an orphaned container still running under id %q)", runTimeout, id)
		}
		if tail, rerr := os.ReadFile(logPath); rerr == nil && len(tail) > 0 {
			lines := strings.Split(strings.TrimSpace(string(tail)), "\n")
			lastLine := lines[len(lines)-1]
			if strings.Contains(lastLine, "level=error") || strings.Contains(lastLine, "ERRO") || strings.Contains(lastLine, "failed") {
				return fmt.Errorf("runc run: %w: %s", err, lastLine)
			}
		}
		return fmt.Errorf("runc run: %w", err)
	}
	return nil
}

func (r *execRunner) Kill(id, signal string) error {
	cmd := r.command("kill", id, signal)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("runc kill: %w: %s", err, stderr.String())
	}
	return nil
}

func (r *execRunner) Delete(id string) error {
	cmd := r.command("delete", "--force", id)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("runc delete: %w: %s", err, stderr.String())
	}
	return nil
}

func (r *execRunner) State(id string) (*runcState, error) {
	cmd := r.command("state", id)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("runc state: %w: %s", err, stderr.String())
	}
	var st runcState
	if err := json.Unmarshal(stdout.Bytes(), &st); err != nil {
		return nil, fmt.Errorf("runc state: invalid JSON: %w", err)
	}
	return &st, nil
}

// Exec executes a command inside a running container and captures its output.
func (r *execRunner) Exec(id string, cmdArgs []string, user, cwd string) (string, string, int, error) {
	args := []string{"exec"}
	if user != "" {
		args = append(args, "-u", user)
	}
	if cwd != "" {
		args = append(args, "--cwd", cwd)
	}
	args = append(args, id)
	args = append(args, cmdArgs...)

	cmd := r.command(args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return stdout.String(), stderr.String(), -1, fmt.Errorf("runc exec: %w: %s", err, stderr.String())
		}
	}
	return stdout.String(), stderr.String(), exitCode, nil
}

// Available reports whether the configured runc binary exists and reports
// a version, which is what health.RuntimeCheckFunc consults.
func (r *execRunner) Available() bool {
	cmd := exec.Command(r.runcPath, "--version")
	return cmd.Run() == nil
}

