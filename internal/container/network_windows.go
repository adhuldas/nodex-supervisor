//go:build windows

package container

// isNetnsMount is a stub for Windows.
func isNetnsMount(path string) bool {
	return false
}
