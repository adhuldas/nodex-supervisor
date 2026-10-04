package wifi

// The sysfs/nmcli tests stub Linux paths; run them as Linux whatever
// machine they're on. darwin_test.go calls the macOS functions directly.
func init() { hostOS = "linux" }
