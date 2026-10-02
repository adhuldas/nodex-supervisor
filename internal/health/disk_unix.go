//go:build !windows

package health

import "syscall"

// diskUsage returns path's filesystem used-percent alongside its raw byte totals.
func diskUsage(path string) (usedPercent float64, totalBytes, usedBytes uint64) {
	if path == "" {
		return 0, 0, 0
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, 0
	}
	total := stat.Blocks * uint64(stat.Bsize)
	free := stat.Bfree * uint64(stat.Bsize)
	if total == 0 {
		return 0, 0, 0
	}
	used := total - free
	return float64(used) / float64(total) * 100, total, used
}
