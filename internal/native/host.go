package native

import "runtime"

// A busy guest with many logical CPUs starves the host's own UI and I/O
// threads, so default to a quarter (rounded up). Overcommit across VMs is fine.
func DefaultCPUs() int { return (runtime.NumCPU() + 3) / 4 }

// WHPX commits all guest RAM at start, so large defaults fail on hosts with
// little free commit; a quarter also avoids swapping during parallel builds.
func DefaultMemMB() int {
	mb := HostMemMB() / 4
	if mb < 2048 {
		mb = 2048
	}
	return mb
}
