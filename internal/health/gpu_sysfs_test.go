package health

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeDRM builds a /sys/class/drm with one entry per card: driver name and
// optionally the gpu_busy_percent file.
func fakeDRM(t *testing.T, cards map[string]struct{ driver, busy string }) {
	t.Helper()
	root := t.TempDir()
	old, oldNV := drmDir, procNvidiaDir
	drmDir, procNvidiaDir = filepath.Join(root, "drm"), filepath.Join(root, "no-nvidia")
	t.Cleanup(func() { drmDir, procNvidiaDir = old, oldNV })

	for card, c := range cards {
		dev := filepath.Join(drmDir, card, "device")
		if err := os.MkdirAll(dev, 0o755); err != nil {
			t.Fatal(err)
		}
		if c.driver != "" {
			drivers := filepath.Join(root, "bus", "drivers", c.driver)
			if err := os.MkdirAll(drivers, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(drivers, filepath.Join(dev, "driver")); err != nil {
				t.Fatal(err)
			}
		}
		if c.busy != "" {
			if err := os.WriteFile(filepath.Join(dev, "gpu_busy_percent"), []byte(c.busy+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestReadGPUFromAMDSysfs(t *testing.T) {
	fakeDRM(t, map[string]struct{ driver, busy string }{
		"card0":      {"amdgpu", "37"},
		"card0-DP-1": {"amdgpu", "99"}, // a connector, not a GPU
		"renderD128": {"amdgpu", "98"},
		"card1":      {"hyperv_drm", ""}, // virtual adapter: ignored
	})
	name, percent := readGPU()
	if name != "AMD GPU (amdgpu)" {
		t.Errorf("name = %q", name)
	}
	if percent == nil || *percent != 37 {
		t.Errorf("percent = %v, want 37 (connectors and render nodes aren't GPUs)", percent)
	}
}

func TestReadGPUWithoutAUsageCounterKeepsTheNameOnly(t *testing.T) {
	// Intel's i915 has no usage file: report the GPU, not a made-up 0%.
	fakeDRM(t, map[string]struct{ driver, busy string }{"card0": {"i915", ""}})
	name, percent := readGPU()
	if name != "Intel GPU (i915)" || percent != nil {
		t.Errorf("name = %q percent = %v", name, percent)
	}
}

func TestReadGPUReportsNothingOnAHostWithNone(t *testing.T) {
	fakeDRM(t, map[string]struct{ driver, busy string }{"card0": {"simpledrm", ""}})
	if name, percent := readGPU(); name != "" || percent != nil {
		t.Errorf("name = %q percent = %v for a framebuffer-only host", name, percent)
	}
	fakeDRM(t, nil)
	if name, percent := readGPU(); name != "" || percent != nil {
		t.Errorf("name = %q percent = %v for a host with no DRM devices", name, percent)
	}
}

func TestReadGPUTakesTheBusiestOfSeveral(t *testing.T) {
	fakeDRM(t, map[string]struct{ driver, busy string }{
		"card0": {"amdgpu", "12"},
		"card1": {"amdgpu", "64"},
	})
	if _, percent := readGPU(); percent == nil || *percent != 64 {
		t.Errorf("percent = %v, want 64", percent)
	}
}

func TestParseNvidiaSMI(t *testing.T) {
	name, percent, ok := parseNvidiaSMI("NVIDIA GeForce RTX 3080, 12\nNVIDIA GeForce RTX 3080, 87\n")
	if !ok || name != "NVIDIA GeForce RTX 3080" || percent != 87 {
		t.Errorf("parseNvidiaSMI = %q %v %v", name, percent, ok)
	}
	if _, _, ok := parseNvidiaSMI("Tesla T4, [N/A]\n"); ok {
		t.Error("a GPU that can't report usage was read as 0%")
	}
	if _, _, ok := parseNvidiaSMI("No devices were found\n"); ok {
		t.Error("parsed a value from nvidia-smi's error text")
	}
}
