package cli

import (
	"fmt"
	"io"
	"os"

	"github.com/huangzheng2016/pvm/internal/daemon"
)

type auditCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// CmdAudit is a read-only health audit: it never starts or stops the VM
// and never touches routes. The exit code is 1 only when a check reports
// fail; warn and skipped do not block the workflow.
func CmdAudit(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: pvm audit <vm>")
		return 2
	}
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	c, err := daemon.Ensure(p)
	if err != nil {
		return fatalf("%v", err)
	}
	var checks []auditCheck
	if err := c.Get("/v1/vms/"+args[0]+"/audit", &checks); err != nil {
		return fatalf("%v", err)
	}
	return printAudit(os.Stdout, checks)
}

func printAudit(w io.Writer, checks []auditCheck) int {
	code := 0
	for _, c := range checks {
		fmt.Fprintf(w, "%s=%s: %s\n", c.Name, c.Status, c.Detail)
		if c.Status == "fail" {
			code = 1
		}
	}
	return code
}
