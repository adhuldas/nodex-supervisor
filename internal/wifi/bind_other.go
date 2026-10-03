//go:build !linux

package wifi

func bindToDevice(fd uintptr, iface string) error {
	return nil
}
