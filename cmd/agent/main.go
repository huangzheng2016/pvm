package main

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/huangzheng2016/pvm/internal/agent"
)

const usage = `usage: pvm <command> [args]

commands:
  agent       run the guest agent service
  map         manage port maps for this VM: add | ls | rm
  skill       print the guest usage guide
`

const (
	agentUsage = "usage: pvm agent"
	skillUsage = "usage: pvm skill"
)

//go:embed SKILL.md
var skillDoc string

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	if len(args) == 1 && isHelp(args[0]) {
		fmt.Fprint(stdout, usage)
		return 0
	}
	switch args[0] {
	case "agent":
		if len(args) == 2 && isHelp(args[1]) {
			fmt.Fprintln(stdout, agentUsage)
			return 0
		}
		if len(args) != 1 {
			return usageError(stderr, agentUsage)
		}
		if runtime.GOOS != "linux" {
			fmt.Fprintln(stderr, "pvm agent runs inside a linux guest")
			return 1
		}
		fmt.Fprintln(stderr, "pvm agent starting")
		if err := agent.NewServer().Run("/"); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	case "map":
		return cmdMap(args[1:], agent.ControlSock, agent.ControlCall, stdout, stderr)
	case "skill":
		if len(args) == 2 && isHelp(args[1]) {
			fmt.Fprintln(stdout, skillUsage)
			return 0
		}
		if len(args) != 1 {
			return usageError(stderr, skillUsage)
		}
		fmt.Fprint(stdout, skillDoc)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		fmt.Fprint(stderr, usage)
		return 2
	}
}

func isHelp(arg string) bool {
	return arg == "help" || arg == "--help" || arg == "-h"
}

func usageError(stderr io.Writer, text string) int {
	fmt.Fprintln(stderr, text)
	return 2
}
