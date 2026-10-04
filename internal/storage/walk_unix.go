//go:build unix

package storage

import (
	"context"
	"io/fs"
	"path/filepath"
	"syscall"
	"time"
)

const (
	// throttleEvery and throttle pause the walk briefly every few thousand
	// entries so a scan of millions of files doesn't starve the host's
	// own workloads of disk I/O.
	throttleEvery = 1000
	throttle      = 2 * time.Millisecond

	// maxHardlinks caps the hard-link bookkeeping. The agent runs under a
	// small memory limit; past the cap extra links are counted again, a
	// slight overcount instead of unbounded memory.
	maxHardlinks = 100_000
)

// walkSize returns the bytes allocated under root, counted the way `du -x`
// does: allocated blocks rather than file lengths, hard-linked files once,
// nothing from other filesystems (overlay mounts under running containers,
// tmpfs, ...) and nothing under the directories in exclude, which were
// measured separately.
//
// seenDirs, when given, records every directory visited by (device, inode)
// and is shared across calls: a directory reachable twice -- a bind mount of
// one path inside another on the same filesystem -- is measured once.
func walkSize(ctx context.Context, root string, dev uint64, exclude map[string]bool, seenDirs map[[2]uint64]struct{}) (uint64, error) {
	var total uint64
	seen := make(map[[2]uint64]struct{})
	visited := 0

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable or vanished: containers and logs churn constantly.
			return nil
		}
		if visited++; visited%throttleEvery == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			time.Sleep(throttle)
		}
		if d.IsDir() && path != root && exclude[path] {
			return fs.SkipDir
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil
		}
		if uint64(st.Dev) != dev {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() && seenDirs != nil {
			key := [2]uint64{uint64(st.Dev), uint64(st.Ino)}
			if _, dup := seenDirs[key]; dup {
				return fs.SkipDir
			}
			if len(seenDirs) < maxHardlinks {
				seenDirs[key] = struct{}{}
			}
		}
		if !d.IsDir() && st.Nlink > 1 {
			key := [2]uint64{uint64(st.Dev), uint64(st.Ino)}
			if _, dup := seen[key]; dup {
				return nil
			}
			if len(seen) < maxHardlinks {
				seen[key] = struct{}{}
			}
		}
		total += uint64(st.Blocks) * 512
		return nil
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// deviceOf returns the id of the filesystem holding path.
func deviceOf(path string) (uint64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Dev), nil
}
