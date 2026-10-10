package main

import (
	"fmt"
	"os"

	"github.com/huangzheng2016/pvm/internal/buildinfo"
	"github.com/huangzheng2016/pvm/internal/cli"
)

const usage = `pvm %s — personal virtual machines

usage: pvm <command> [args]

vm:
  create        create (and start) a vm
  list          list vms
  start|stop    start/stop a vm
  rm            stop and delete a vm
  ssh           ssh into a vm: pvm ssh dev1 [cmd...]

image:
  image fetch   download a base image (ubuntu:24.04, debian:13)
  image list    list downloaded images

daemon:
  daemon start|stop|status|restart
  daemon run                foreground (used by launchd/systemd)
  daemon install|uninstall  register as a login service

other:
  version       print version

planned (M2+): map, exit, route, dns, kasm, agent, gateway, exit-agent
`

func main() {
	if len(os.Args) < 2 {
		fmt.Printf(usage, buildinfo.Version)
		os.Exit(2)
	}
	args := os.Args[2:]
	var code int
	switch os.Args[1] {
	case "daemon":
		code = cli.CmdDaemon(args)
	case "image":
		code = cli.CmdImage(args)
	case "create":
		code = cli.CmdCreate(args)
	case "list", "ls":
		code = cli.CmdList(args)
	case "start":
		code = cli.CmdStart(args)
	case "stop":
		code = cli.CmdStop(args)
	case "rm":
		code = cli.CmdRm(args)
	case "ssh":
		code = cli.CmdSSH(args)
	case "exit":
		code = cli.CmdExit(args)
	case "exit-agent":
		code = cli.CmdExitAgent(args)
	case "version", "--version", "-v":
		fmt.Println(buildinfo.Version)
	case "map", "route", "dns", "kasm", "agent", "gateway":
		fmt.Printf("%s: not implemented yet (M2+)\n", os.Args[1])
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		fmt.Printf(usage, buildinfo.Version)
		os.Exit(2)
	}
	os.Exit(code)
}
