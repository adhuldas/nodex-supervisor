//go:build !windows

package container

import "syscall"

// isNetnsMount reports whether path is a live network namespace mount in
// this process's mount namespace.
func isNetnsMount(path string) bool {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return false
	}
	return uint32(st.Type) == nsfsMagic
}
