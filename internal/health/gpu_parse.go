package health

import (
	"strconv"
	"strings"
)

// parseNvidiaSMI parses `nvidia-smi --query-gpu=name,utilization.gpu
// --format=csv,noheader,nounits`: one "name, percent" line per GPU. It
// returns the first GPU's name and the busiest one's utilization; ok is
// false when no line carries a number ("[N/A]" when the GPU can't say).
func parseNvidiaSMI(out string) (name string, percent float64, ok bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		i := strings.LastIndex(line, ",")
		if i < 0 {
			continue
		}
		util, err := strconv.ParseFloat(strings.TrimSpace(line[i+1:]), 64)
		if err != nil {
			continue
		}
		if name == "" {
			name = strings.TrimSpace(line[:i])
		}
		percent, ok = max(percent, util), true
	}
	return name, min(percent, 100), ok
}

// drmGPUNames gives the kernel's DRM drivers a readable GPU name; the
// kernel exposes the driver, not a product name, and the PCI id database
// that would translate ids isn't on a small image.
var drmGPUNames = map[string]string{
	"amdgpu":   "AMD GPU",
	"radeon":   "AMD Radeon GPU",
	"i915":     "Intel GPU",
	"xe":       "Intel GPU",
	"nouveau":  "NVIDIA GPU",
	"nvidia":   "NVIDIA GPU",
	"panfrost": "Arm Mali GPU",
	"lima":     "Arm Mali GPU",
	"etnaviv":  "Vivante GPU",
	"msm":      "Qualcomm Adreno GPU",
	"v3d":      "Broadcom VideoCore GPU",
	"vc4":      "Broadcom VideoCore GPU",
	"tegra":    "NVIDIA Tegra GPU",
}

// drmNotGPU are display-only or virtual adapters: a server's BMC video
// chip or a VM's framebuffer isn't a GPU worth showing on the dashboard.
var drmNotGPU = map[string]bool{
	"simpledrm": true, "simple-framebuffer": true, "ofdrm": true, "vesadrm": true, "efidrm": true,
	"ast": true, "mgag200": true, "bochs": true, "bochs-drm": true, "cirrus": true, "qxl": true,
	"virtio_gpu": true, "vmwgfx": true, "vboxvideo": true, "hyperv_drm": true, "udl": true,
	"gma500_gfx": true,
}

// drmGPUName returns the dashboard name for a DRM driver, and false for
// adapters that aren't GPUs.
func drmGPUName(driver string) (string, bool) {
	if driver == "" || drmNotGPU[driver] {
		return "", false
	}
	if name, ok := drmGPUNames[driver]; ok {
		return name + " (" + driver + ")", true
	}
	return driver, true
}
