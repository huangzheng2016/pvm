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
  clone         clone a stopped vm: pvm clone dev1 dev2 [--inherit-ports] [--no-start]
  list          list vms
  start|stop    start/stop a vm
  rm            stop and delete a vm (asks for confirmation; -y|--force to skip)
  ssh           ssh into a vm: pvm ssh dev1 [cmd...]
  snapshot      ram snapshots: create <name> | ls [name] | rm <name>
                (create needs a running vm; rm needs the vm stopped)
  exec          run a command via the guest agent (no network needed): pvm exec dev1 cmd...
  logs          stream a vm console: pvm logs dev1 [--follow] [--tail N]
  desktop       open the vm's native xfce desktop: pvm desktop dev1 [--open] [--stop] [--size WxH]

network:
  map           map ports: pvm map dev1 8080:80 [--udp] | ls | rm <port> [--udp]
  route         vm default route: pvm route dev1 --via us|direct
                split rules: pvm route add direct|proxy|internal [--domain-suffix|--domain-prefix|--domain-keyword|--cidr] <v> [--via <exit>] [--dns <srv>]
                inspect: pvm route ls | pvm route rm <index>
  dns           inspect fake-ip mappings: pvm dns ls
  exit          manage exit nodes: add (--transport auto=probe&pick fastest) | set | ls | rm | test [--speed]
  exit-agent    run the vps exit agent: pvm exit-agent --listen :18443 --token <tok>

image:
  image fetch   download a base image (ubuntu:24.04, debian:13, alpine:3.21)
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
  upgrade       self-upgrade to the latest release: pvm upgrade [--check] [--to <tag>] [-y]

common choices:
  images        ubuntu:24.04 | ubuntu:22.04 | debian:13 | debian:12 | alpine:3.20 | alpine:3.21
  transports    udp (default, wireguard) | tcp (wireguard over tcp)
  nat backends  auto | nft | iptables | userspace (no root, no icmp)
  route via     direct | <exit-name>  (this host: us, us-tcp)

create options:
  --image IMG   ubuntu:24.04 | ubuntu:22.04 | debian:13 | debian:12 | alpine:3.20 | alpine:3.21
  --distro D --version V  same as --image D:V (conflict with --image errors)
  --name NAME   vm name (required)
  --user USER   guest login user (required for non-interactive use)
  --cpus N      vcpus (default 2)
  --mem MB      memory in mb (default 4096)
  --disk GB     disk size in gb (default 20)
  --mount H:G   extra host:guest mount; ~/.pvm/share is always at /share
  --tz TZ       iana timezone, e.g. America/Los_Angeles
  --locale LOC  guest locale (default en_US.UTF-8)
  --git-email E optional neutral git identity (never inherited from host)
  --git-name N  optional neutral git name (requires --git-email)
  --smbios-spoof  spoof dmi as a dell inspiron laptop
  --no-start    create disk and metadata without starting the vm
  --no-wait     return before cloud-init finishes

examples:
  pvm create --image ubuntu:24.04 --name dev1 --user dev
  pvm route dev1 --via us
  pvm ssh dev1 'curl -s ipinfo.io'
  pvm desktop dev1
  pvm map dev1 8080:80
  pvm snapshot create dev1 && pvm start dev1
`

func main() {
	if len(os.Args) < 2 {
		fmt.Printf(usage, buildinfo.Version)
		os.Exit(2)
	}
	args := os.Args[2:]
	switch os.Args[1] {
	case "upgrade", "version", "--version", "-v", "skill", "exit-agent", "agent", "gateway", "__snapshot-pipe-out", "__snapshot-pipe-in":
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
