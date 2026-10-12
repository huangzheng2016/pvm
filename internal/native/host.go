package native

import "runtime"

// HostCPUs returns the host's logical CPU count. vCPUs are ordinary host
// threads, so overcommitting them across VMs is the intended default.
func HostCPUs() int { return runtime.NumCPU() }

// DefaultMemMB is the per-VM memory default: half of the host's physical
// ram (lazy allocation makes it safe to overcommit, but handing every VM
// the host total invites swap death under parallel builds).
func DefaultMemMB() int {
	mb := HostMemMB() / 2
	if mb < 2048 {
		mb = 2048
	}
	return mb
}
