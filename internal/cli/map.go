package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/huangzheng2016/pvm/internal/daemon"
)

type mapView struct {
	VM        string `json:"vm"`
	VMIP      string `json:"vm_ip"`
	HostPort  int    `json:"host_port"`
	PoolPort  int    `json:"pool_port"`
	GuestPort int    `json:"guest_port"`
	Proto     string `json:"proto"`
}

func CmdMap(args []string) int {
	const usage = "<vm> <hostPort>:<guestPort> [--udp] | ls | rm <port> [--udp]"
	fs := newFlagSet("map", usage)
	udp := fs.Bool("udp", false, "map UDP instead of TCP")
	var flags, positionals []string
	parseFlags := true
	for _, arg := range args {
		if parseFlags && arg == "--" {
			parseFlags = false
		} else if parseFlags && strings.HasPrefix(arg, "-") && arg != "-" {
			flags = append(flags, arg)
		} else {
			positionals = append(positionals, arg)
		}
	}
	if err := fs.Parse(append(append(flags, "--"), positionals...)); err != nil {
		return 1
	}
	args = fs.Args()
	if len(args) == 0 || (args[0] == "ls" && (len(args) != 1 || *udp)) || (args[0] != "ls" && len(args) != 2) {
		return fatalf("usage: pvm map %s", usage)
	}
	proto := "tcp"
	if *udp {
		proto = "udp"
	}
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	if args[0] == "ls" {
		c, err := daemon.Ensure(p)
		if err != nil {
			return fatalf("%v", err)
		}
		var maps []mapView
		if err := c.Get("/v1/maps", &maps); err != nil {
			return fatalf("%v", err)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "VM\tPROTO\tHOST\tGUEST")
		for _, m := range maps {
			fmt.Fprintf(tw, "%s\t%s\t127.0.0.1:%d\t%s:%d\n", m.VM, m.Proto, m.HostPort, m.VMIP, m.GuestPort)
		}
		tw.Flush()
		return 0
	}
	if args[0] == "rm" {
		c, err := daemon.Ensure(p)
		if err != nil {
			return fatalf("%v", err)
		}
		if err := c.Delete(fmt.Sprintf("/v1/maps/%s/%s", args[1], proto), nil); err != nil {
			return fatalf("%v", err)
		}
		fmt.Printf("removed map %s/%s\n", args[1], proto)
		return 0
	}
	vmName := args[0]
	hostS, guestS, ok := strings.Cut(args[1], ":")
	if !ok {
		return fatalf("usage: pvm map %s", usage)
	}
	host, err1 := strconv.Atoi(hostS)
	guest, err2 := strconv.Atoi(guestS)
	if err1 != nil || err2 != nil {
		return fatalf("ports must be numbers: %q", args[1])
	}
	c, err := daemon.Ensure(p)
	if err != nil {
		return fatalf("%v", err)
	}
	body := map[string]any{"vm": vmName, "host_port": host, "guest_port": guest, "proto": proto}
	var out mapView
	if err := c.Post("/v1/maps", body, &out); err != nil {
		return fatalf("map: %v", err)
	}
	fmt.Printf("mapped 127.0.0.1:%d/%s -> %s(%s):%d\n", out.HostPort, out.Proto, out.VM, out.VMIP, out.GuestPort)
	return 0
}
