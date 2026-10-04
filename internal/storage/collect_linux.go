package storage

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type target struct {
	path     string
	category string
}

// targets are the directories measured, in order. The first match wins
// for any path measured twice, and a directory inside one already measured
// is left out of the later (outer) walk, so nothing is counted twice.
func targets(ctx context.Context, dataDir string) []target {
	return []target{
		{dockerRoot(ctx), CategoryDocker},
		{"/var/lib/containerd", CategoryDocker},
		{dataDir, CategoryDocker},
		{"/var/log", CategoryLogs},
		{"/opt", CategoryApplications},
		{"/usr", CategoryOS},
		{"/home", CategoryOther},
		{"/root", CategoryOther},
	}
}

// dockerRoot is where Docker keeps its data; "docker info" knows when
// that's been moved off the default.
func dockerRoot(ctx context.Context) string {
	if _, err := exec.LookPath("docker"); err == nil {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.DockerRootDir}}").Output()
		if p := strings.TrimSpace(string(out)); err == nil && filepath.IsAbs(p) {
			return p
		}
	}
	return "/var/lib/docker"
}

// collect measures the targets that sit on the same filesystem as
// diskPath. A directory on another filesystem (a separate disk for
// /var/lib/docker, say) isn't part of this disk's usage, so it's left out
// rather than distorting the totals.
func collect(ctx context.Context, diskPath, dataDir string) (*Breakdown, error) {
	var fsStat syscall.Statfs_t
	if err := syscall.Statfs(diskPath, &fsStat); err != nil {
		return nil, err
	}
	used := (fsStat.Blocks - fsStat.Bfree) * uint64(fsStat.Bsize)

	diskDev, err := deviceOf(diskPath)
	if err != nil {
		return nil, err
	}

	var entries []Entry
	measured := make(map[string]bool)
	for _, t := range targets(ctx, dataDir) {
		root, err := filepath.EvalSymlinks(t.path)
		if err != nil {
			continue // not on this host
		}
		if insideAny(root, measured) {
			continue
		}
		dev, err := deviceOf(root)
		if err != nil || dev != diskDev {
			continue
		}
		size, err := walkSize(ctx, root, dev, measured)
		if err != nil {
			return nil, err
		}
		measured[root] = true
		entries = append(entries, Entry{Path: t.path, Category: t.category, Bytes: size})
	}

	b := assemble(entries, used, time.Now().UTC())
	return &b, nil
}

// insideAny reports whether path is, or is under, one of roots.
func insideAny(path string, roots map[string]bool) bool {
	for r := range roots {
		if rel, err := filepath.Rel(r, path); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return true
		}
	}
	return false
}
