package swap

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// Create makes Path a sizeMB swap file, active now and at boot. An
// existing Path is replaced, so this also resizes it.
func Create(ctx context.Context, sizeMB int) error {
	if err := validateSize(sizeMB); err != nil {
		return err
	}

	// Space the old file frees counts as available.
	var existingMB uint64
	if info, err := os.Stat(Path); err == nil {
		existingMB = uint64(info.Size()) >> 20
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil {
		return fmt.Errorf("swap: checking free disk space: %w", err)
	}
	freeMB := st.Bavail*uint64(st.Bsize)>>20 + existingMB
	if need := uint64(sizeMB + headroomMB); freeMB < need {
		return fmt.Errorf("swap: not enough disk space: %d MB free, %d MB needed (%d MB swap + %d MB headroom)", freeMB, need, sizeMB, headroomMB)
	}

	if procSwaps, err := os.ReadFile("/proc/swaps"); err == nil && isActive(string(procSwaps)) {
		// Fails (ENOMEM) when RAM can't absorb what's swapped out; the old
		// swap file is then left exactly as it was.
		if err := run(ctx, "swapoff", Path); err != nil {
			return fmt.Errorf("swap: turning off the current swap file: %w", err)
		}
	}
	if err := os.Remove(Path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("swap: removing old swap file: %w", err)
	}

	if err := writeSwapFile(ctx, sizeMB, false); err != nil {
		os.Remove(Path)
		return err
	}
	if err := run(ctx, "swapon", Path); err != nil {
		// Some filesystems reject fallocate'd (unwritten) swap files;
		// a fully written one works on them.
		if err := writeSwapFile(ctx, sizeMB, true); err != nil {
			os.Remove(Path)
			return err
		}
		if err := run(ctx, "swapon", Path); err != nil {
			os.Remove(Path)
			return fmt.Errorf("swap: enabling swap file: %w", err)
		}
	}

	fstab, err := os.ReadFile(fstabPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("swap: active now, but reading %s failed, so it won't survive a reboot: %w", fstabPath, err)
	}
	if updated := withFstabEntry(string(fstab)); updated != string(fstab) {
		tmp := fstabPath + ".nodexa.tmp"
		if err := os.WriteFile(tmp, []byte(updated), 0o644); err != nil {
			return fmt.Errorf("swap: active now, but updating %s failed: %w", fstabPath, err)
		}
		if err := os.Rename(tmp, fstabPath); err != nil {
			os.Remove(tmp)
			return fmt.Errorf("swap: active now, but updating %s failed: %w", fstabPath, err)
		}
	}
	return nil
}

func writeSwapFile(ctx context.Context, sizeMB int, zeroFill bool) error {
	os.Remove(Path)
	var err error
	if !zeroFill {
		err = run(ctx, "fallocate", "-l", strconv.Itoa(sizeMB)+"M", Path)
	}
	if zeroFill || err != nil {
		err = run(ctx, "dd", "if=/dev/zero", "of="+Path, "bs=1M", "count="+strconv.Itoa(sizeMB))
	}
	if err != nil {
		return fmt.Errorf("swap: writing swap file: %w", err)
	}
	if err := os.Chmod(Path, 0o600); err != nil {
		return fmt.Errorf("swap: securing swap file: %w", err)
	}
	if err := run(ctx, "mkswap", Path); err != nil {
		return fmt.Errorf("swap: formatting swap file: %w", err)
	}
	return nil
}

func run(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return nil
}
