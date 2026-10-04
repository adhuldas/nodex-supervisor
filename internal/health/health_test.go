package health

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func writeTempFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}

func TestReadCPUPercentFirstCallReturnsZero(t *testing.T) {
	orig := procStatPath
	procStatPath = writeTempFile(t, "cpu  100 0 100 800 0 0 0 0 0 0\n")
	defer func() { procStatPath = orig }()

	c := &Checker{}
	if got := c.readCPUPercent(); got != 0 {
		t.Fatalf("expected 0 on first sample, got %v", got)
	}
}

func TestReadCPUPercentComputesDeltaAcrossCalls(t *testing.T) {
	path := writeTempFile(t, "cpu  100 0 100 800 0 0 0 0 0 0\n")
	orig := procStatPath
	procStatPath = path
	defer func() { procStatPath = orig }()

	c := &Checker{}
	c.readCPUPercent() // seed previous sample

	// total advances by 200 ticks, idle by 50 -> 75% busy.
	if err := os.WriteFile(path, []byte("cpu  250 0 100 850 0 0 0 0 0 0\n"), 0o600); err != nil {
		t.Fatalf("rewrite stat file: %v", err)
	}
	got := c.readCPUPercent()
	if got != 75 {
		t.Fatalf("expected 75%%, got %v", got)
	}
}

func TestReadCPUPercentMissingFileReturnsZero(t *testing.T) {
	orig := procStatPath
	procStatPath = filepath.Join(t.TempDir(), "does-not-exist")
	defer func() { procStatPath = orig }()

	c := &Checker{}
	if got := c.readCPUPercent(); got != 0 {
		t.Fatalf("expected 0 for missing file, got %v", got)
	}
}

func TestReadTemperatureCParsesMillidegrees(t *testing.T) {
	dir := t.TempDir()
	zone := filepath.Join(dir, "thermal_zone0")
	if err := os.MkdirAll(zone, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(zone, "temp"), []byte("47213\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	orig := thermalZoneGlob
	thermalZoneGlob = filepath.Join(dir, "thermal_zone*", "temp")
	defer func() { thermalZoneGlob = orig }()

	got := readTemperatureC()
	if got == nil {
		t.Fatal("expected non-nil temperature")
	}
	if *got != 47.213 {
		t.Fatalf("expected 47.213, got %v", *got)
	}
}

func TestReadTemperatureCMissingReturnsNil(t *testing.T) {
	orig := thermalZoneGlob
	thermalZoneGlob = filepath.Join(t.TempDir(), "thermal_zone*", "temp")
	defer func() { thermalZoneGlob = orig }()

	if got := readTemperatureC(); got != nil {
		t.Fatalf("expected nil for missing thermal zone, got %v", *got)
	}
}

func TestCheckPopulatesNewFields(t *testing.T) {
	c := NewChecker("", nil, nil)
	report := c.Check()

	// /proc/stat needs two samples; macOS's top measures one directly.
	if runtime.GOOS != "darwin" && report.CPUPercent != 0 {
		t.Fatalf("expected 0 CPUPercent on first Check, got %v", report.CPUPercent)
	}
	// TemperatureC is expected to be nil on the (non-Linux) test machine;
	// this just asserts Check doesn't panic wiring it in.
	_ = report.TemperatureC
}

func TestReadLoadAvgAndUptime(t *testing.T) {
	dir := t.TempDir()
	loadPath := filepath.Join(dir, "loadavg")
	uptimePath := filepath.Join(dir, "uptime")
	if err := os.WriteFile(loadPath, []byte("0.52 0.31 0.12 1/234 5678\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(uptimePath, []byte("3725.41 7000.00\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldLoad, oldUptime := procLoadAvgPath, procUptimePath
	procLoadAvgPath, procUptimePath = loadPath, uptimePath
	t.Cleanup(func() { procLoadAvgPath, procUptimePath = oldLoad, oldUptime })

	if l1, l5, l15 := readLoadAvg(); l1 != 0.52 || l5 != 0.31 || l15 != 0.12 {
		t.Fatalf("load = %v %v %v", l1, l5, l15)
	}
	if got := readUptime(); got != 3725 {
		t.Fatalf("uptime = %d, want 3725", got)
	}
}
