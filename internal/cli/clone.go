package cli

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/huangzheng2016/pvm/internal/daemon"
)

type srcMapView struct {
	VM        string `json:"vm"`
	HostPort  int    `json:"host_port"`
	GuestPort int    `json:"guest_port"`
	Proto     string `json:"proto"`
}

type mapPlan struct {
	HostPort  int
	GuestPort int
	Proto     string
	Note      string
}

// planMaps decides which of the source VM's port maps the clone gets.
// mode "none" skips all, "inherit" keeps each (remapping busy ports),
// "interactive" asks via prompt. prompt receives the map and a suggested
// alternate port; it returns (port, true) to keep or (0, false) to skip.
func planMaps(maps []srcMapView, mode string, free func(proto string, port int) bool, prompt func(m srcMapView, suggested int) (int, bool)) ([]mapPlan, error) {
	var out []mapPlan
	for _, m := range maps {
		proto := m.Proto
		if proto == "" {
			proto = "tcp"
		}
		switch mode {
		case "none":
			continue
		case "inherit":
			hp := m.HostPort
			note := ""
			if !free(proto, hp) {
				alt := nextFreePort(proto, hp+1, free)
				if alt == 0 {
					return nil, fmt.Errorf("no free port for inherited map %d:%d/%s", m.HostPort, m.GuestPort, proto)
				}
				note = fmt.Sprintf("%d busy, remapped to %d", hp, alt)
				hp = alt
			}
			out = append(out, mapPlan{HostPort: hp, GuestPort: m.GuestPort, Proto: proto, Note: note})
		case "interactive":
			port, ok := prompt(m, m.HostPort+1)
			if ok {
				out = append(out, mapPlan{HostPort: port, GuestPort: m.GuestPort, Proto: proto})
			}
		}
	}
	return out, nil
}

func nextFreePort(proto string, prefer int, free func(string, int) bool) int {
	for p := prefer; p <= 65535; p++ {
		if free(proto, p) {
			return p
		}
	}
	for p := 1024; p < prefer && p < 65535; p++ {
		if free(proto, p) {
			return p
		}
	}
	return 0
}

func probePortFree(proto string, port int) bool {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if proto == "udp" {
		c, err := net.ListenPacket("udp", addr)
		if err != nil {
			return false
		}
		c.Close()
		return true
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// promptMapInherit implements the interactive per-map decision:
// y = keep (same port, or a suggested free one if busy), N = skip,
// a number = remap to that host port.
func promptMapInherit(r *bufio.Reader, free func(string, int) bool) func(srcMapView, int) (int, bool) {
	return func(m srcMapView, suggested int) (int, bool) {
		fmt.Printf("  inherit map %d:%d/%s? [y/N/<new port>] ", m.HostPort, m.GuestPort, m.Proto)
		line, _ := r.ReadString('\n')
		ans := strings.ToLower(strings.TrimSpace(line))
		switch ans {
		case "", "n", "no":
			return 0, false
		case "y", "yes":
			if free(m.Proto, m.HostPort) {
				return m.HostPort, true
			}
			alt := nextFreePort(m.Proto, suggested, free)
			if alt == 0 {
				fmt.Println("    no free port; skipped")
				return 0, false
			}
			fmt.Printf("    %d busy; using %d\n", m.HostPort, alt)
			return alt, true
		default:
			port, err := strconv.Atoi(ans)
			if err != nil || port < 1024 || port > 65535 {
				fmt.Println("    invalid port; skipped")
				return 0, false
			}
			if !free(m.Proto, port) {
				fmt.Printf("    %d busy; skipped\n", port)
				return 0, false
			}
			return port, true
		}
	}
}

func CmdClone(args []string) int {
	fs := newFlagSet("clone", "<src> <new> [--inherit-ports|--no-inherit-ports] [--no-start]")
	inherit := fs.Bool("inherit-ports", false, "inherit all port maps (busy ports auto-remapped)")
	noInherit := fs.Bool("no-inherit-ports", false, "do not inherit any port maps (default when non-interactive)")
	noStart := fs.Bool("no-start", false, "clone but do not start")
	if err := fs.Parse(partitionFlags(args)); err != nil {
		return 2
	}
	if fs.NArg() < 2 {
		fs.Usage()
		return 2
	}
	if *inherit && *noInherit {
		return fatalf("--inherit-ports and --no-inherit-ports are mutually exclusive")
	}
	src, newName := fs.Arg(0), fs.Arg(1)

	p, err := mustPaths()
	if err != nil {
		return fatalf("%v", err)
	}
	c, err := daemon.Ensure(p)
	if err != nil {
		return fatalf("%v", err)
	}

	var allMaps []srcMapView
	if err := c.Get("/v1/maps", &allMaps); err != nil {
		return fatalf("list maps: %v", err)
	}
	var srcMaps []srcMapView
	for _, m := range allMaps {
		if m.VM == src {
			srcMaps = append(srcMaps, m)
		}
	}

	mode := "none"
	switch {
	case *inherit:
		mode = "inherit"
	case !*noInherit && len(srcMaps) > 0 && stdinIsTTY():
		mode = "interactive"
	}
	if mode == "interactive" {
		fmt.Printf("source %s has %d port map(s):\n", src, len(srcMaps))
	}
	plans, err := planMaps(srcMaps, mode, probePortFree, promptMapInherit(bufio.NewReader(os.Stdin), probePortFree))
	if err != nil {
		return fatalf("port maps: %v", err)
	}

	maps := make([]map[string]any, 0, len(plans))
	for _, pl := range plans {
		maps = append(maps, map[string]any{"host_port": pl.HostPort, "guest_port": pl.GuestPort, "proto": pl.Proto})
	}
	body := map[string]any{"new_name": newName, "start": !*noStart, "maps": maps}

	var out struct {
		Spec struct {
			Name string `json:"name"`
			IP   string `json:"ip"`
		} `json:"spec"`
		Running bool `json:"running"`
	}
	if err := c.Post("/v1/vms/"+src+"/clone", body, &out); err != nil {
		return fatalf("clone: %v", err)
	}
	fmt.Printf("cloned %s -> %s (ip=%s)\n", src, out.Spec.Name, out.Spec.IP)
	for _, pl := range plans {
		note := ""
		if pl.Note != "" {
			note = " (" + pl.Note + ")"
		}
		fmt.Printf("  map %d:%d/%s%s\n", pl.HostPort, pl.GuestPort, pl.Proto, note)
	}
	if out.Running {
		fmt.Println("starting; guest identity (hostname/machine-id/ssh keys) is regenerated on first boot")
	}
	return 0
}
