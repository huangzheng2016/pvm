package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/huangzheng2016/pvm/internal/agent"
)

const mapUsage = "usage: pvm map add <hostPort>:<guestPort> [--udp] | pvm map ls | pvm map rm <hostPort> [--udp]"

const mapControlTimeout = 30 * time.Second

type controlCaller func(sock, action string, req, resp any, timeout time.Duration) error

func cmdMap(args []string, sock string, call controlCaller, stdout, stderr io.Writer) int {
	if len(args) == 1 && isHelp(args[0]) {
		fmt.Fprintln(stdout, mapUsage)
		return 0
	}
	fs := flag.NewFlagSet("map", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, mapUsage) }
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
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fs.Usage()
		return 2
	}
	args = fs.Args()
	if len(args) == 0 {
		return usageError(stderr, mapUsage)
	}
	proto := "tcp"
	if *udp {
		proto = "udp"
	}
	switch args[0] {
	case "ls":
		if len(args) != 1 || *udp {
			return usageError(stderr, mapUsage)
		}
		var resp agent.MapListResponse
		if err := call(sock, agent.ControlMapList, nil, &resp, mapControlTimeout); err != nil {
			fmt.Fprintf(stderr, "map ls: %v\n", err)
			return 1
		}
		tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "PROTO\tHOST\tGUEST")
		for _, m := range resp.Maps {
			fmt.Fprintf(tw, "%s\t127.0.0.1:%d\t%d\n", m.Proto, m.HostPort, m.GuestPort)
		}
		tw.Flush()
		return 0
	case "add":
		if len(args) != 2 {
			return usageError(stderr, mapUsage)
		}
		hostPort, guestPort, err := parseMapPorts(args[1])
		if err != nil {
			fmt.Fprintln(stderr, err)
			return usageError(stderr, mapUsage)
		}
		req := agent.MapAddRequest{HostPort: hostPort, GuestPort: guestPort, Proto: proto}
		var resp agent.MapAddResponse
		if err := call(sock, agent.ControlMapAdd, &req, &resp, mapControlTimeout); err != nil {
			fmt.Fprintf(stderr, "map add: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "mapped 127.0.0.1:%d/%s -> guest port %d\n", resp.Map.HostPort, resp.Map.Proto, resp.Map.GuestPort)
		return 0
	case "rm":
		if len(args) != 2 {
			return usageError(stderr, mapUsage)
		}
		hostPort, err := parsePort(args[1], "host")
		if err != nil || hostPort == 0 {
			fmt.Fprintln(stderr, "host port must be 1-65535")
			return usageError(stderr, mapUsage)
		}
		req := agent.MapRemoveRequest{HostPort: hostPort, Proto: proto}
		if err := call(sock, agent.ControlMapRemove, &req, nil, mapControlTimeout); err != nil {
			fmt.Fprintf(stderr, "map rm: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "removed map %d/%s\n", hostPort, proto)
		return 0
	default:
		return usageError(stderr, mapUsage)
	}
}

func parseMapPorts(value string) (int, int, error) {
	hostText, guestText, ok := strings.Cut(value, ":")
	if !ok {
		return 0, 0, fmt.Errorf("map must be <hostPort>:<guestPort>")
	}
	hostPort, hostErr := parsePort(hostText, "host")
	guestPort, guestErr := parsePort(guestText, "guest")
	if hostErr != nil || (hostPort != 0 && hostPort < 1024) {
		return 0, 0, fmt.Errorf("host port must be 0 or 1024-65535")
	}
	if guestErr != nil || guestPort == 0 {
		return 0, 0, fmt.Errorf("guest port must be 1-65535")
	}
	return hostPort, guestPort, nil
}

func parsePort(value, name string) (int, error) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 0 || port > 65535 {
		return 0, fmt.Errorf("%s port must be a number in range", name)
	}
	return port, nil
}
