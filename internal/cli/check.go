package cli

import (
	"fmt"
	"net/url"

	"github.com/huangzheng2016/pvm/internal/daemon"
	"github.com/huangzheng2016/pvm/internal/netcheck"
)

func CmdCheck(args []string) int {
	args = scanJSONFlag(args)
	if len(args) != 1 {
		return failErr(fmt.Errorf("usage: pvm check <vm> [--json]"))
	}
	p, err := mustPaths()
	if err != nil {
		return failErr(err)
	}
	c, err := daemon.Ensure(p)
	if err != nil {
		return failErr(err)
	}
	var rep netcheck.Report
	if err := c.Post("/v1/vms/"+url.PathEscape(args[0])+"/check", nil, &rep); err != nil {
		return failErr(err)
	}
	ok := rep.Authorizes(rep.BootID, rep.RouteHash)
	if jsonOutput {
		status := "completed"
		if !ok {
			status = "failed"
		}
		if err := writeJSONOut(map[string]any{"status": status, "authorized": ok, "data": rep}); err != nil {
			return failErr(err)
		}
	} else {
		fmt.Printf("check %s (policy %s, run %s)\n", rep.VM, rep.Policy, rep.RunID)
		for _, item := range rep.Checks {
			mark := map[string]string{netcheck.StatePassed: "PASS", netcheck.StateFailed: "FAIL", netcheck.StateUnknown: "UNKNOWN"}[item.State]
			fmt.Printf("  [% -7s] %s: %s\n", mark, item.Title, sanitizeText(item.Evidence))
			if item.Remedy != "" && item.State != netcheck.StatePassed {
				fmt.Printf("             remedy: %s\n", item.Remedy)
			}
		}
	}
	if !ok {
		return 1
	}
	return 0
}
