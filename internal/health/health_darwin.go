package health

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// readPlatform fills in what the /proc readers can't on macOS: memory,
// swap, CPU, load averages and uptime, from sysctl, vm_stat and top.
func (c *Checker) readPlatform(r *Report) {
	if out, ok := runTool("sysctl", "-n", "hw.memsize"); ok {
		if total, err := strconv.ParseUint(strings.TrimSpace(out), 10, 64); err == nil && total > 0 {
			if vm, ok := runTool("vm_stat"); ok {
				if used, ok := parseVMStat(vm); ok {
					used = min(used, total)
					r.MemTotalBytes, r.MemUsedBytes = total, used
					r.MemUsedPercent = float64(used) / float64(total) * 100
				}
			}
		}
	}
	if out, ok := runTool("sysctl", "-n", "vm.swapusage"); ok {
		if total, used, ok := parseSwapUsage(out); ok {
			r.SwapTotalBytes, r.SwapUsedBytes = &total, &used
		}
	}
	if out, ok := runTool("sysctl", "-n", "vm.loadavg"); ok {
		if l1, l5, l15, ok := parseLoadAvg(out); ok {
			r.LoadAvg1, r.LoadAvg5, r.LoadAvg15 = l1, l5, l15
		}
	}
	if out, ok := runTool("sysctl", "-n", "kern.boottime"); ok {
		if boot, ok := parseBootTime(out); ok && boot > 0 {
			if up := time.Now().Unix() - boot; up > 0 {
				r.UptimeSeconds = uint64(up)
			}
		}
	}
	// Two one-second samples: the first is an average since boot.
	if out, ok := runTool("top", "-l", "2", "-n", "0", "-s", "1"); ok {
		if pct, ok := parseTopCPU(out); ok {
			r.CPUPercent = pct
		}
	}
}

func runTool(name string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).Output()
	return string(out), err == nil
}
