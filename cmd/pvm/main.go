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
  clone         clone a stopped vm: pvm clone dev1 dev2 [--inherit-ports]
  list          list vms
  start|stop    start/stop a vm
  rm            stop and delete a vm (asks for confirmation; -y to skip)
  ssh           ssh into a vm: pvm ssh dev1 [cmd...]
  snapshot      ram snapshots: create <name> | ls [name] | rm <name>
  exec          run a command via the guest agent (no network needed): pvm exec dev1 cmd...
  logs          stream a vm console: pvm logs dev1 [--follow] [--tail N]
  kasm          open a vm's web desktop: pvm kasm dev1 [--open]

network:
  map           map ports: pvm map dev1 8080:80 [--udp] | ls | rm <port>
  route         route vm/direct/internal rules: pvm route dev1 --via us|direct
  dns           inspect fake-ip mappings: pvm dns ls
  exit          manage exit nodes: add | set | ls | rm | test
  exit-agent    run the vps exit agent: pvm exit-agent --listen :18443 --token <tok>

image:
  image fetch   download a base image (ubuntu:24.04, debian:13)
  image build   bake a golden image (docker + kasm preinstalled)
  image list    list base and golden images

daemon:
  daemon start|stop|status|restart
  daemon run                foreground (used by launchd/systemd)
  daemon install|uninstall  register as a login service

other:
  version       print version
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
	case "clone":
		code = cli.CmdClone(args)
	case "ssh":
		code = cli.CmdSSH(args)
	case "logs":
		code = cli.CmdLogs(args)
	case "map":
		code = cli.CmdMap(args)
	case "route":
		code = cli.CmdRoute(args)
	case "dns":
		code = cli.CmdDNS(args)
	case "exit":
		code = cli.CmdExit(args)
	case "exit-agent":
		code = cli.CmdExitAgent(args)
	case "snapshot":
		code = cli.CmdSnapshot(args)
	case "__snapshot-pipe-out":
		code = cli.CmdSnapshotPipeOut(args)
	case "__snapshot-pipe-in":
		code = cli.CmdSnapshotPipeIn(args)
	case "exec":
		code = cli.CmdExec(args)
	case "agent":
		code = cli.CmdAgent(args)
	case "version", "--version", "-v":
		fmt.Println(buildinfo.Version)
	case "kasm":
		code = cli.CmdKasm(args)
	case "gateway":
		fmt.Println("gateway is managed automatically by the pvm daemon; no manual action is required")
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		fmt.Printf(usage, buildinfo.Version)
		os.Exit(2)
	}
	os.Exit(code)
}
