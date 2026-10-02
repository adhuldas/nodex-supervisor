package update

import "syscall"

func mountFS(source, target, fstype string, readOnly bool) error {
	var flags uintptr
	if readOnly {
		flags |= syscall.MS_RDONLY
	}
	return syscall.Mount(source, target, fstype, flags, "")
}

func unmountFS(target string) error {
	return syscall.Unmount(target, 0)
}
