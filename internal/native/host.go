package native

import "runtime"

// vCPUs are ordinary host threads, so overcommitting them across VMs is intentional.
func HostCPUs() int { return runtime.NumCPU() }

// Lazy allocation permits memory overcommit, but giving every VM the host
// total can trigger swapping during parallel builds.
func DefaultMemMB() int {
	mb := HostMemMB() / 2
	if mb < 2048 {
		mb = 2048
	}
	return mb
}
