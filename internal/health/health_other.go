//go:build !darwin && !linux

package health

// readPlatform: Windows reports disk only for now.
func (c *Checker) readPlatform(r *Report) {}
