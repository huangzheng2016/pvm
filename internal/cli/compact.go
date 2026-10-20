package cli

import (
	"fmt"

	"github.com/huangzheng2016/pvm/internal/daemon"
)

type compactView struct {
	BeforeBytes    int64 `json:"before_bytes"`
	AfterBytes     int64 `json:"after_bytes"`
	ReclaimedBytes int64 `json:"reclaimed_bytes"`
	Changed        bool  `json:"changed"`
}

func CmdCompact(args []string) int {
	if len(args) != 1 {
		return fatalf("usage: pvm compact <name>")
	}
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	c, err := daemon.Ensure(p)
	if err != nil {
		return fatalf("%v", err)
	}
	var out compactView
	if err := c.Post("/v1/vms/"+args[0]+"/compact", nil, &out); err != nil {
		return fatalf("compact: %v", err)
	}
	if !out.Changed {
		fmt.Printf("%s: no smaller disk found; original kept (host usage %s)\n", args[0], formatDiskBytes(out.BeforeBytes))
		return 0
	}
	fmt.Printf("%s: reclaimed %s (host usage %s -> %s); virtual capacity unchanged\n",
		args[0], formatDiskBytes(out.ReclaimedBytes), formatDiskBytes(out.BeforeBytes), formatDiskBytes(out.AfterBytes))
	return 0
}

func formatDiskBytes(n int64) string {
	switch {
	case n >= 1<<40:
		return fmt.Sprintf("%.1fT", float64(n)/(1<<40))
	case n >= 1<<30:
		return fmt.Sprintf("%.1fG", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1fM", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fK", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
