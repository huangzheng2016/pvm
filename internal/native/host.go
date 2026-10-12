package native

import "runtime"

// HostCPUs returns the host's logical CPU count. vCPUs are ordinary host
// threads, so overcommitting them across VMs is the intended default.
func HostCPUs() int { return runtime.NumCPU() }
