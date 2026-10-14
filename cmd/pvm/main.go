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
  clone         clone a stopped vm: pvm clone dev1 dev2 [--inherit-ports] [--no-start] [--no-wait]
  list          list vms
  start|stop    start/stop a vm (start waits for provisioning; --no-wait returns early)
  rm            stop and delete a vm (asks for confirmation; -y|--force to skip)
  resize        grow a stopped vm's disk (grow-only): pvm resize dev1 40
  ssh           ssh into a vm: pvm ssh dev1 [cmd...]
  snapshot      ram snapshots: create <name> | ls [name] | rm <name>
                (create needs a running vm; rm needs the vm stopped)
  exec          run a command via the guest agent (no network needed): pvm exec dev1 cmd...
  logs          stream a vm console: pvm logs dev1 [--follow] [--tail N]
  desktop       open the vm's native xfce desktop: pvm desktop dev1 [--open] [--stop] [--size WxH]

network:
  map           map ports: pvm map dev1 8080:80 [--udp] | ls | rm <port> [--udp]
  route         vm default route: pvm route dev1 --via my-exit|direct
                split rules: pvm route add direct|proxy|internal [--domain-suffix|--domain-prefix|--domain-keyword|--cidr] <v> [--via <exit>] [--dns <srv>]
                inspect: pvm route ls | pvm route rm <index>
  dns           inspect fake-ip mappings: pvm dns ls
  exit          manage exit nodes: add (--transport auto=probe&pick fastest) | set | ls | rm | test [--speed]
                export a standard wireguard client: pvm exit client-config my-exit [-o phone.conf]
  exit-agent    run the vps exit agent: pvm exit-agent --listen :18443 --token <tok>

image:
  image fetch   download a base image (ubuntu:26.04, debian:13)
  image build   bake a golden image (docker + xfce desktop preinstalled; ubuntu/debian only)
                flags: --keep-vm --cpus N --mem MB --disk GB
  image list    list base and golden images

daemon:
  daemon start|stop|status|restart
  daemon run                foreground (used by launchd/systemd)
  daemon install|uninstall  register as a login service

other:
  version       print version
  skill         print the embedded pvm skill doc (install into agent skills)
  security      print the embedded security/risk-control guide
  upgrade       self-upgrade and restart the daemon: pvm upgrade [--check] [--to <tag>] [--no-restart] [-y]

common choices:
  images        ubuntu:26.04 | ubuntu:24.04 | debian:13 | debian:12
  transports    udp (default, wireguard) | tcp (wireguard over tcp)
  nat backends  auto | nft | iptables | userspace (no root, no icmp)
  route via     direct | <exit-name>  (for example: my-exit)

create options:
  --image IMG   ubuntu:26.04 | ubuntu:24.04 | debian:13 | debian:12
  --distro D --version V  same as --image D:V (conflict with --image errors)
  --name NAME   vm name (required)
  --user USER   guest login user (required for non-interactive use)
  --cpus N      vcpus (default: all host cores)
  --mem MB      memory in mb (default: half of host ram; idle pages return via balloon)
  --disk GB     disk size in gb (default 20)
  --mount H:G   extra host:guest mount; ~/.pvm/share is always at /share
  --tz TZ       iana timezone, e.g. America/Los_Angeles
  --locale LOC  guest locale (default en_US.UTF-8)
  --git-email E optional neutral git identity (never inherited from host)
  --git-name N  optional neutral git name (requires --git-email)
  --smbios-spoof  spoof dmi as a dell optiplex desktop
  --no-start    create disk and metadata without starting the vm
  --no-wait     return before cloud-init finishes

examples:
  pvm create --image ubuntu:24.04 --name dev1 --user dev
  pvm route dev1 --via my-exit
  pvm ssh dev1 'curl -s ipinfo.io'
  pvm desktop dev1
  pvm map dev1 8080:80
  pvm snapshot create dev1 && pvm start dev1
`

func helpRequested(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "--help" || arg == "-h" {
			return true
		}
	}
	return false
}

func main() {
	if len(os.Args) < 2 {
		fmt.Printf(usage, buildinfo.Version)
		os.Exit(2)
	}
	args := os.Args[2:]
	if os.Args[1] == "help" || os.Args[1] == "--help" || os.Args[1] == "-h" || helpRequested(args) {
		fmt.Printf(usage, buildinfo.Version)
		return
	}
	switch os.Args[1] {
	case "upgrade", "version", "--version", "-v", "skill", "security", "exit-agent", "agent", "gateway", "__snapshot-pipe-out", "__snapshot-pipe-in":
	default:
		cli.MaybeNotifyUpgrade()
	}
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
	case "resize":
		code = cli.CmdResize(args)
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
	case "skill":
		code = cli.CmdSkill(args)
	case "security":
		code = cli.CmdSecurity(args)
	case "upgrade":
		code = cli.CmdUpgrade(args)
	case "desktop":
		code = cli.CmdDesktop(args)
	case "gateway":
		fmt.Println("gateway is managed automatically by the pvm daemon; no manual action is required")
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		fmt.Printf(usage, buildinfo.Version)
		os.Exit(2)
	}
	os.Exit(code)
}
