//go:build !linux

package swap

import (
	"context"
	"fmt"
	"runtime"
)

// Create isn't supported off Linux: macOS manages swap itself, and
// Windows' page file is configured through the system settings.
func Create(ctx context.Context, sizeMB int) error {
	if err := validateSize(sizeMB); err != nil {
		return err
	}
	return fmt.Errorf("swap: creating swap is only supported on Linux hosts, not %s", runtime.GOOS)
}

// Delete is a no-op on non-Linux hosts.
func Delete(ctx context.Context) error {
	return nil
}
