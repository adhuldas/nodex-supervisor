package container

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/events"
)

// fakeRunc is a runc stand-in keeping each container's status in
// <root>/<id>: run sets it to running, delete removes it, and a test
// writes "stopped" to simulate the process exiting on its own.
const fakeRunc = `#!/bin/sh
root=$2; shift 2
case "$1" in
  state) [ -f "$root/$2" ] || exit 1; printf '{"id":"%s","status":"%s"}' "$2" "$(cat "$root/$2")" ;;
  run)   eval id=\${$#}; echo running > "$root/$id"; echo "$id" >> "$root/runs" ;;
  delete) rm -f "$root/$3" ;;
esac
`

func newRestartTestManager(t *testing.T, containers map[string]string) (*NodexaContainerManager, string) {
	t.Helper()
	runcPath := filepath.Join(t.TempDir(), "runc")
	if err := os.WriteFile(runcPath, []byte(fakeRunc), 0o755); err != nil {
		t.Fatal(err)
	}
	containerDir, runcRoot := t.TempDir(), t.TempDir()
	mgr := NewManager(containerDir, "", t.TempDir(), t.TempDir(), runcPath, runcRoot, events.NewBus(100))
	for name, policy := range containers {
		dir := filepath.Join(containerDir, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		cfg := `{"linux":{"cgroupsPath":"nodexa/` + name + `"}}`
		if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
		md := &metadata{Name: name, Network: "host", Restart: policy, StartedAt: time.Now()}
		if err := writeMetadata(dir, md); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runcRoot, name), []byte("stopped"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return mgr, runcRoot
}

func runs(t *testing.T, runcRoot string) []string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(runcRoot, "runs"))
	return strings.Fields(string(b))
}

func TestRestartExitedFollowsPolicy(t *testing.T) {
	mgr, runcRoot := newRestartTestManager(t, map[string]string{
		"nginx":    "unless-stopped",
		"firmware": "always",
		"job":      "on-failure",
		"oneshot":  "no",
		"legacy":   "",
	})
	// Stopped on purpose: Stop deleted its runc state.
	if err := os.Remove(filepath.Join(runcRoot, "firmware")); err != nil {
		t.Fatal(err)
	}

	mgr.restartExited(time.Now())

	got := strings.Join(runs(t, runcRoot), " ")
	for _, want := range []string{"nginx", "job"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s not restarted (runs: %q)", want, got)
		}
	}
	for _, not := range []string{"firmware", "oneshot", "legacy"} {
		if strings.Contains(got, not) {
			t.Errorf("%s restarted (runs: %q)", not, got)
		}
	}
}

func TestRestartExitedBacksOff(t *testing.T) {
	mgr, runcRoot := newRestartTestManager(t, map[string]string{"nginx": "always"})
	crash := func() {
		if err := os.WriteFile(filepath.Join(runcRoot, "nginx"), []byte("stopped"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now()
	mgr.restartExited(now) // attempt 1, next allowed after 1s
	crash()
	mgr.restartExited(now.Add(500 * time.Millisecond))
	if n := len(runs(t, runcRoot)); n != 1 {
		t.Fatalf("restarted %d times within the backoff, want 1", n)
	}
	mgr.restartExited(now.Add(1 * time.Second)) // attempt 2, next after 2s more
	crash()
	mgr.restartExited(now.Add(2 * time.Second))
	if n := len(runs(t, runcRoot)); n != 2 {
		t.Fatalf("restarted %d times, want 2", n)
	}
	mgr.restartExited(now.Add(3 * time.Second))
	if n := len(runs(t, runcRoot)); n != 3 {
		t.Fatalf("restarted %d times, want 3", n)
	}
}

func TestRestartExitedSkipsMidDeploy(t *testing.T) {
	mgr, runcRoot := newRestartTestManager(t, map[string]string{"nginx": "always"})
	mgr.setTransient(NodexaContainer{Name: "nginx", State: StateContainerCreated})

	mgr.restartExited(time.Now())

	if got := runs(t, runcRoot); len(got) != 0 {
		t.Fatalf("restarted mid-deploy: %v", got)
	}
}
