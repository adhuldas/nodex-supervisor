package container

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProcessCgroup(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "42"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "42", "cgroup"), []byte("0::/system.slice/nodexa/web\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := procRoot
	procRoot = dir
	t.Cleanup(func() { procRoot = old })

	if got := processCgroup(42); got != "/system.slice/nodexa/web" {
		t.Fatalf("processCgroup = %q", got)
	}
	if got := processCgroup(43); got != "" {
		t.Fatalf("missing pid: processCgroup = %q, want empty", got)
	}
}
