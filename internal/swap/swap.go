// Package swap creates the swap file the dashboard's "Create swap" action
// asks for. It manages a single file, Path; swap partitions or other swap
// files the host already has are left alone and keep counting towards the
// swap the heartbeat reports.
package swap

import (
	"fmt"
	"strings"
)

const (
	// Path is the swap file this package owns.
	Path = "/swapfile"

	// MinSizeMB and MaxSizeMB bound a requested size (nodexa-backend
	// enforces the same range).
	MinSizeMB = 256
	MaxSizeMB = 65536

	// headroomMB is disk space left free after the file is written.
	headroomMB = 512

	fstabPath = "/etc/fstab"
	fstabLine = Path + " none swap sw 0 0"
)

func validateSize(sizeMB int) error {
	if sizeMB < MinSizeMB || sizeMB > MaxSizeMB {
		return fmt.Errorf("swap: size %d MB out of range (%d-%d MB)", sizeMB, MinSizeMB, MaxSizeMB)
	}
	return nil
}

// withFstabEntry returns fstab with the line that enables Path at boot,
// unchanged if Path already has an entry.
func withFstabEntry(fstab string) string {
	for _, line := range strings.Split(fstab, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == Path {
			return fstab
		}
	}
	if fstab != "" && !strings.HasSuffix(fstab, "\n") {
		fstab += "\n"
	}
	return fstab + fstabLine + "\n"
}

// isActive reports whether /proc/swaps content lists Path.
func isActive(procSwaps string) bool {
	for _, line := range strings.Split(procSwaps, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == Path {
			return true
		}
	}
	return false
}
