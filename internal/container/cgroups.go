package container

import (
	"bufio"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// cgroupRoot and procRoot are overridable for tests.
var (
	cgroupRoot = "/sys/fs/cgroup"
	procRoot   = "/proc"
)

// processCgroup returns pid's cgroup v2 path ("/system.slice/nodexa/web"),
// or "" if it can't be read.
func processCgroup(pid int) string {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok && p != "" && p != "/" {
			return p
		}
	}
	return ""
}

// readCgroupStats reads cgroup v2 accounting files for the given
// cgroup path (relative to cgroupRoot, e.g. "nodexa/my-container").
// Missing files (container not running, or cgroup v1 host) yield zero
// values rather than errors, since stats are best-effort.
func readCgroupStats(cgroupPath string) (cpuSeconds float64, memBytes uint64, memLimit uint64, pids int) {
	dir := filepath.Join(cgroupRoot, cgroupPath)

	if usec := readCPUUsageUsec(filepath.Join(dir, "cpu.stat")); usec > 0 {
		cpuSeconds = float64(usec) / 1e6
	}
	memBytes = readUintFile(filepath.Join(dir, "memory.current"))
	if limit := readUintFile(filepath.Join(dir, "memory.max")); limit > 0 {
		memLimit = limit
	}
	pids = int(readUintFile(filepath.Join(dir, "pids.current")))

	return cpuSeconds, memBytes, memLimit, pids
}

func readCPUUsageUsec(path string) uint64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && fields[0] == "usage_usec" {
			v, err := strconv.ParseUint(fields[1], 10, 64)
			if err == nil {
				return v
			}
		}
	}
	return 0
}

func readUintFile(path string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	s := strings.TrimSpace(string(b))
	if s == "max" || s == "" {
		return 0
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}
