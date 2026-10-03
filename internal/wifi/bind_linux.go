//go:build linux

package wifi

import (
	"syscall"
)

func bindToDevice(fd uintptr, iface string) error {
	if iface == "" {
		return nil
	}
	// SO_BINDTODEVICE binds the socket to a specific network interface.
	// We ignore EPERM/EACCES (e.g. unprivileged user in tests or containers).
	err := syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, iface)
	if err == syscall.EPERM || err == syscall.EACCES {
		return nil
	}
	return err
}
