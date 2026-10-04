//go:build darwin

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

// targets are the directories measured on macOS, in order. The first match wins
// for any path measured twice, and a directory inside one already measured
// is left out of the later (outer) walk, so nothing is counted twice.
func targets(ctx context.Context, dataDir string) []target {
	var list []target

	// Docker / container runtimes:
	// Docker Desktop, OrbStack, Colima, or Podman storage.
	if root := dockerRoot(ctx); root != "" {
		list = append(list, target{root, CategoryDocker})
	}
	if dataDir != "" {
		list = append(list, target{dataDir, CategoryDocker})
	}
	list = append(list,
		target{"/var/lib/docker", CategoryDocker},
		target{"/var/lib/containerd", CategoryDocker},
	)
	if matches, err := filepath.Glob("/Users/*/Library/Containers/com.docker.docker"); err == nil {
		for _, m := range matches {
			list = append(list, target{m, CategoryDocker})
		}
	}
	if matches, err := filepath.Glob("/Users/*/.orbstack"); err == nil {
		for _, m := range matches {
			list = append(list, target{m, CategoryDocker})
		}
	}
	if matches, err := filepath.Glob("/Users/*/.colima"); err == nil {
		for _, m := range matches {
			list = append(list, target{m, CategoryDocker})
		}
	}
	if matches, err := filepath.Glob("/Users/*/.local/share/containers"); err == nil {
		for _, m := range matches {
			list = append(list, target{m, CategoryDocker})
		}
	}

	// Logs: system and application logs
	list = append(list,
		target{"/Library/Logs", CategoryLogs},
		target{"/var/log", CategoryLogs},
	)
	if matches, err := filepath.Glob("/Users/*/Library/Logs"); err == nil {
		for _, m := range matches {
			list = append(list, target{m, CategoryLogs})
		}
	}

	// Applications
	list = append(list,
		target{"/Applications", CategoryApplications},
		target{"/System/Applications", CategoryApplications},
		target{"/opt", CategoryApplications},
		target{"/usr/local", CategoryApplications},
	)

	// OS: system components, frameworks and unix binaries
	list = append(list,
		target{"/Library", CategoryOS},
		target{"/usr", CategoryOS},
		target{"/System/Library", CategoryOS},
	)

	// Other: users and homes
	list = append(list,
		target{"/Users", CategoryOther},
	)

	return list
}

// dockerRoot checks if docker CLI points to a host root directory.
func dockerRoot(ctx context.Context) string {
	if _, err := exec.LookPath("docker"); err == nil {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.DockerRootDir}}").Output()
		if p := strings.TrimSpace(string(out)); err == nil && filepath.IsAbs(p) {
			return p
		}
	}
	return ""
}

// collect measures the targets that sit on the same filesystem as
// diskPath on macOS.
func collect(ctx context.Context, diskPath, dataDir string) (*Breakdown, error) {
	var fsStat syscall.Statfs_t
	if diskPath == "" || syscall.Statfs(diskPath, &fsStat) != nil {
		if err := syscall.Statfs("/", &fsStat); err != nil {
			return nil, err
		}
		diskPath = "/"
	}
	used := (fsStat.Blocks - fsStat.Bfree) * uint64(fsStat.Bsize)

	diskDev, err := deviceOf(diskPath)
	if err != nil {
		diskDev, err = deviceOf("/")
		if err != nil {
			return nil, err
		}
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
