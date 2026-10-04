package container

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDockerImageItemName(t *testing.T) {
	cases := []struct {
		it   dockerImageItem
		want string
	}{
		{dockerImageItem{Repository: "nginx", Tag: "alpine", ID: "abc"}, "nginx:alpine"},
		{dockerImageItem{Repository: "nginx", Tag: "<none>", ID: "abc"}, "nginx"},
		{dockerImageItem{Repository: "<none>", Tag: "<none>", ID: "abc"}, "abc"},
	}
	for _, c := range cases {
		if got := c.it.name(); got != c.want {
			t.Errorf("name(%+v) = %q, want %q", c.it, got, c.want)
		}
	}
}

func TestEngineUsageTotals(t *testing.T) {
	u := &EngineUsage{
		Volumes:        []VolumeUsage{{Name: "a", SizeBytes: 10}, {Name: "b", SizeBytes: 30}},
		DanglingImages: []ImageUsage{{Name: "x", SizeBytes: 5}},
		Logs:           []LogUsage{{Container: "c", SizeBytes: 7}},
	}
	u.total()
	if u.VolumesBytes != 40 || u.Volumes[0].Name != "b" || u.DanglingBytes != 5 || u.LogsBytes != 7 {
		t.Errorf("totals = %+v", u)
	}
	if u.RunningImages == nil {
		t.Error("empty list should be non-nil")
	}
}

func TestRuncUsageAndClearLogs(t *testing.T) {
	containerDir, volumesDir := t.TempDir(), t.TempDir()
	mgr := NewManager(containerDir, "", t.TempDir(), volumesDir, "/nonexistent/runc", t.TempDir(), nil)

	bundle := filepath.Join(containerDir, "web")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMetadata(bundle, &metadata{Name: "web", Image: "nginx"}); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(bundle, "container.log")
	if err := os.WriteFile(logPath, []byte("hello log\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(volumesDir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(volumesDir, "data", "f"), make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}

	u, err := mgr.Usage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if u.Engine != "runc" || u.VolumesBytes != 100 || u.LogsBytes != 10 || len(u.DanglingImages) != 0 {
		t.Fatalf("usage = %+v", u)
	}

	if _, err := mgr.ClearLogs(context.Background(), "nope"); err == nil {
		t.Error("unknown container should fail")
	}
	freed, err := mgr.ClearLogs(context.Background(), "")
	if err != nil || freed != 10 {
		t.Fatalf("freed = %d, err = %v", freed, err)
	}
	if fi, _ := os.Stat(logPath); fi.Size() != 0 {
		t.Errorf("log size = %d after clear", fi.Size())
	}
}
