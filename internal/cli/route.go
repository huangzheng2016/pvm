package cli

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/huangzheng2016/pvm/internal/daemon"
)

type ruleView struct {
	Type    string `json:"type"`
	Pattern string `json:"pattern"`
	Action  string `json:"action"`
	Exit    string `json:"exit,omitempty"`
	DNS     string `json:"dns,omitempty"`
}

type routeListView struct {
	Rules       []ruleView        `json:"rules"`
	VMExits     map[string]string `json:"vm_exits"`
	DefaultExit string            `json:"default_exit"`
}

func CmdRoute(args []string) int {
	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	if len(args) < 1 {
		return routeUsage()
	}
	c, err := daemon.Ensure(p)
	if err != nil {
		return fatalf("%v", err)
	}
	switch args[0] {
	case "ls":
		var v routeListView
		if err := c.Get("/v1/routes", &v); err != nil {
			return fatalf("%v", err)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "INDEX\tTYPE\tPATTERN\tACTION\tVIA")
		for i, r := range v.Rules {
			via := r.Exit
			if r.Action == "internal" {
				via = r.DNS
				if via == "" {
					via = "(host dns)"
				}
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\n", i, r.Type, r.Pattern, r.Action, via)
		}
		for vm, exit := range v.VMExits {
			action := "proxy"
			if exit == "direct" {
				action = "direct"
			}
			fmt.Fprintf(tw, "\tvm\t%s\t%s\t%s\n", vm, action, exit)
		}
		def := v.DefaultExit
		if def == "" {
			def = "direct"
		}
		fmt.Fprintf(tw, "\tcluster-default\t*\troute\t%s\n", def)
		tw.Flush()
		return 0
	case "add":
		return routeAdd(c, args[1:])
	case "rm":
		if len(args) < 2 {
			return routeUsage()
		}
		if _, err := strconv.Atoi(args[1]); err != nil {
			return fatalf("index must be a number")
		}
		if err := c.Delete("/v1/routes/"+args[1], nil); err != nil {
			return fatalf("%v", err)
		}
		fmt.Printf("removed rule %s\n", args[1])
		return 0
	case "default":
		fs := flag.NewFlagSet("route default", flag.ContinueOnError)
		via := fs.String("via", "", "exit-node name, or direct")
		if err := fs.Parse(args[1:]); err != nil {
			return fatalf("%v", err)
		}
		if *via == "" {
			return routeUsage()
		}
		var resp map[string]any
		if err := c.Post("/v1/routes/default", map[string]string{"via": *via}, &resp); err != nil {
			return fatalf("%v", err)
		}
		fmt.Printf("cluster default route -> %s\n", *via)
		fmt.Println("config pushed; the gateway converges within a few seconds.")
		return 0
	default:
		fs := flag.NewFlagSet("route", flag.ContinueOnError)
		via := fs.String("via", "", "exit-node name, or direct")
		if err := fs.Parse(args[1:]); err != nil {
			return fatalf("%v", err)
		}
		if *via == "" {
			return routeUsage()
		}
		var resp map[string]any
		if err := c.Post("/v1/vms/"+args[0]+"/route", map[string]string{"via": *via}, &resp); err != nil {
			return fatalf("%v", err)
		}
		fmt.Printf("%s: default route -> %s\n", args[0], *via)
		if restarted, _ := resp["restarted"].(bool); restarted {
			fmt.Println("vm was running; it was stopped and restarted to apply the route, and is ssh-ready again.")
		} else {
			fmt.Println("config pushed; the gateway converges within a few seconds. Live connections whose route changed have been reset and will reconnect via the new route.")
		}
		if note, _ := resp["note"].(string); note != "" {
			fmt.Println(note)
		}
		return 0
	}
}

func routeAdd(c *daemon.Client, args []string) int {
	if len(args) < 1 || (args[0] != "direct" && args[0] != "proxy" && args[0] != "internal") {
		return routeUsage()
	}
	action := args[0]
	fs := flag.NewFlagSet("route add", flag.ContinueOnError)
	suffix := fs.String("domain-suffix", "", "match domain suffix (example.com covers a.example.com)")
	prefix := fs.String("domain-prefix", "", "match domain prefix")
	keyword := fs.String("domain-keyword", "", "match domain substring")
	cidr := fs.String("cidr", "", "match destination CIDR (10.0.0.0/8)")
	via := fs.String("via", "", "exit-node name (required for proxy)")
	dns := fs.String("dns", "", "internal resolver host[:port] (required for internal)")
	if err := fs.Parse(args[1:]); err != nil {
		return fatalf("%v", err)
	}
	typ, pattern := "", ""
	for _, e := range []struct{ t, v string }{
		{"domain-suffix", *suffix}, {"domain-prefix", *prefix},
		{"domain-keyword", *keyword}, {"cidr", *cidr},
	} {
		if e.v != "" {
			if typ != "" {
				return fatalf("only one match type per rule")
			}
			typ, pattern = e.t, e.v
		}
	}
	if typ == "" {
		return routeUsage()
	}
	if action == "proxy" && *via == "" {
		return fatalf("proxy rules need --via <exit>")
	}
	body := map[string]string{"type": typ, "pattern": pattern, "action": action, "via": *via, "dns": *dns}
	var resp map[string]any
	if err := c.Post("/v1/routes", body, &resp); err != nil {
		return fatalf("%v", err)
	}
	target := *via
	if action == "internal" {
		target = *dns
		if target == "" {
			target = "(host dns)"
		}
	}
	fmt.Printf("rule %v: %s %s -> %s %s\n", resp["index"], typ, pattern, action, target)
	return 0
}

func routeUsage() int {
	fmt.Fprintln(os.Stderr, `usage:
  pvm route ls
  pvm route add direct|proxy [--domain-suffix|--domain-prefix|--domain-keyword|--cidr] <value> [--via <exit>]
  pvm route add internal [--domain-suffix|--domain-prefix|--domain-keyword] <value> [--dns <host[:port]>]  (no --dns = resolve via host DNS)
  pvm route rm <index>
  pvm route <vm> --via <exit|direct>
  pvm route default --via <exit|direct>  (cluster-wide fallback for unmatched traffic)`)
	return 2
}
