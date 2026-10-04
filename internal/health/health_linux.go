package health

// readPlatform: Linux's CPU, memory and load come from /proc (see Check);
// the GPU comes from the NVIDIA driver and the kernel's DRM devices.
func (c *Checker) readPlatform(r *Report) {
	r.GPUName, r.GPUPercent = readGPU()
}
