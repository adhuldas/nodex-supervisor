package health

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	drmDir        = "/sys/class/drm"
	procNvidiaDir = "/proc/driver/nvidia"
)

var drmCardName = regexp.MustCompile(`^card\d+$`)

// readGPU returns the first GPU's name and the busiest GPU's utilization,
// read on Linux from the NVIDIA driver (nvidia-smi) and the kernel's DRM
// devices (AMD publishes gpu_busy_percent). percent is nil, not 0, when
// the host has no GPU or none that reports usage -- Intel's i915 has no
// usage counter in sysfs -- since 0% is a real reading.
func readGPU() (name string, percent *float64) {
	var busiest float64
	have := false

	// The proprietary NVIDIA driver has no usage counter in sysfs. The
	// directory check keeps hosts without it from running nvidia-smi.
	if _, err := os.Stat(procNvidiaDir); err == nil {
		if n, p, ok := nvidiaSMI(); ok {
			name, busiest, have = n, p, true
		}
	}

	cards, _ := os.ReadDir(drmDir)
	for _, c := range cards {
		if !drmCardName.MatchString(c.Name()) {
			continue
		}
		dev := filepath.Join(drmDir, c.Name(), "device")
		link, err := os.Readlink(filepath.Join(dev, "driver"))
		if err != nil {
			continue
		}
		driverName, ok := drmGPUName(filepath.Base(link))
		if !ok {
			continue
		}
		if name == "" {
			name = driverName
		}
		if b, err := os.ReadFile(filepath.Join(dev, "gpu_busy_percent")); err == nil {
			if v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64); err == nil {
				busiest, have = max(busiest, v), true
			}
		}
	}

	if have {
		busiest = min(max(busiest, 0), 100)
		percent = &busiest
	}
	return name, percent
}

func nvidiaSMI() (name string, percent float64, ok bool) {
	bin, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return "", 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--query-gpu=name,utilization.gpu", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return "", 0, false
	}
	return parseNvidiaSMI(string(out))
}
