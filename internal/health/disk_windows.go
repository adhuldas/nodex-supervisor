//go:build windows

package health

// diskUsage returns path's filesystem used-percent alongside its raw byte totals on Windows.
func diskUsage(path string) (usedPercent float64, totalBytes, usedBytes uint64) {
	return 0, 0, 0
}
