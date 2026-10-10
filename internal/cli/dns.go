package cli

import (
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/huangzheng2016/pvm/internal/daemon"
)

// CmdDNS implements `pvm dns ls`: show the gateway's live fake-ip table.
func CmdDNS(args []string) int {
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	if len(args) != 1 || args[0] != "ls" {
		fmt.Fprintln(os.Stderr, "usage: pvm dns ls")
		return 2
	}
	c, err := daemon.Ensure(p)
	if err != nil {
		return fatalf("%v", err)
	}
	var table map[string]string
	if err := c.Get("/v1/dns", &table); err != nil {
		return fatalf("%v", err)
	}
	doms := make([]string, 0, len(table))
	for d := range table {
		doms = append(doms, d)
	}
	sort.Strings(doms)
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "DOMAIN\tFAKE-IP")
	for _, d := range doms {
		fmt.Fprintf(tw, "%s\t%s\n", d, table[d])
	}
	tw.Flush()
	return 0
}
