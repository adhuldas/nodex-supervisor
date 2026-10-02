package main

import (
	"log"
	"math"
	"runtime"
	"time"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/backend"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/container"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/health"
)

// appDiskInterval is how often an app's storage is re-measured: it walks
// the app's whole bundle, too slow to repeat every heartbeat on a small
// board, and changes slowly anyway.
const appDiskInterval = 10 * time.Minute

type appDisk struct {
	bytes      uint64
	measuredAt time.Time
}

// appUsageTracker works out each app's share of CPU, memory and storage
// for the heartbeat. CPU is the change in the app's cgroup CPU time since
// the previous heartbeat, so an app's first report shows 0%.
type appUsageTracker struct {
	prevCPU map[string]float64
	prevAt  time.Time
	disk    map[string]appDisk
}

func (t *appUsageTracker) sample(mgr *container.NodexaContainerManager, report health.Report) []backend.AppUsage {
	containers, err := mgr.List()
	if err != nil {
		log.Printf("warning: listing containers for app usage: %v", err)
		return nil
	}
	now := time.Now()
	elapsed := now.Sub(t.prevAt).Seconds()
	cpuCapacity := elapsed * float64(runtime.NumCPU())
	cpu := make(map[string]float64, len(containers))
	disk := make(map[string]appDisk, len(containers))

	apps := make([]backend.AppUsage, 0, len(containers))
	for _, c := range containers {
		app := backend.AppUsage{Name: c.Name}
		if st, err := mgr.Stats(c.Name); err == nil {
			cpu[c.Name] = st.CPUUsageSeconds
			if prev, ok := t.prevCPU[c.Name]; ok && !t.prevAt.IsZero() && cpuCapacity > 0 && st.CPUUsageSeconds >= prev {
				app.CPUPercent = percent(st.CPUUsageSeconds-prev, cpuCapacity)
			}
			app.MemBytes = st.MemoryUsageBytes
			app.MemPercent = percent(float64(st.MemoryUsageBytes), float64(report.MemTotalBytes))
		}
		d, ok := t.disk[c.Name]
		if !ok || now.Sub(d.measuredAt) >= appDiskInterval {
			if bytes, err := mgr.DiskUsage(c.Name); err == nil {
				d = appDisk{bytes: bytes, measuredAt: now}
			}
		}
		disk[c.Name] = d
		app.DiskBytes = d.bytes
		app.DiskPercent = percent(float64(d.bytes), float64(report.DiskTotalBytes))
		apps = append(apps, app)
	}
	t.prevCPU, t.prevAt, t.disk = cpu, now, disk
	return apps
}

// percent returns part/whole as a percentage rounded to 0.1, capped at 100.
func percent(part, whole float64) float64 {
	if whole <= 0 {
		return 0
	}
	return math.Min(100, math.Round(part/whole*1000)/10)
}
