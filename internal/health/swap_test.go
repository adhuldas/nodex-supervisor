package health

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadSwapUsage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "meminfo")
	os.WriteFile(p, []byte("MemTotal:  8000000 kB\nSwapTotal: 2097148 kB\nSwapFree:  2096124 kB\n"), 0o644)
	old := procMemInfoPath
	procMemInfoPath = p
	defer func() { procMemInfoPath = old }()
	total, used := readSwapUsage()
	if total == nil || *total != 2097148*1024 || *used != 1024*1024 {
		t.Fatalf("got %v %v", total, used)
	}
	os.WriteFile(p, []byte("MemTotal:  8000000 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n"), 0o644)
	if total, used = readSwapUsage(); total == nil || *total != 0 || *used != 0 {
		t.Fatalf("no swap: got %v %v", total, used)
	}
	procMemInfoPath = filepath.Join(t.TempDir(), "missing")
	if total, _ = readSwapUsage(); total != nil {
		t.Fatal("missing meminfo should report nil")
	}
}
