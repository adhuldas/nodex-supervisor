//go:build !darwin

package health

// readPlatform: Linux is covered by the /proc readers; Windows reports
// disk only for now.
func (c *Checker) readPlatform(r *Report) {}
