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
  list|ls       list vms; STATUS is stopped, starting (<stage>), ready or failed (gateways: running)
  start|stop    start/stop a vm (start waits for provisioning; --no-wait returns early; stop needs the name)
  rm            stop and delete a vm (asks for confirmation; -y|--force to skip)
  resize        grow a stopped vm's disk (grow-only): pvm resize dev1 40
  compact       reclaim unused host disk space (vm must be stopped): pvm compact dev1
  mount         dynamic host mounts: pvm mount [dev1] [--profile coherent|readahead] [--read-only] /host:/guest | ls [dev1] | rm [dev1] /guest
  ssh           ssh into a vm: pvm ssh [-t|-T] [--yes] [dev1] [cmd...]; inside a workspace it starts in the matching directory
  snapshot      ram snapshots: create <name> | ls [name] | rm <name>
                (create needs a running vm; rm needs the vm stopped)
  exec          run a command via the guest agent (no network needed): pvm exec [--yes] [dev1] cmd...
  logs          stream a vm console: pvm logs [dev1] [--follow] [--tail N]
  desktop       open the vm's native xfce desktop: pvm desktop [dev1] [--open] [--stop] [--size WxH] [--public] [--yes]
  app           show one vm app as a host window (xpra): pvm app [dev1] -- gedit | pvm app [dev1] https://example.com
                [--name ID] [--kiosk] [--no-attach] [--yes]; pvm app ls [vm] | stop [dev1] <id>|--all | attach [dev1]
                pvm app config [dev1] [--clipboard off|to-guest|to-host|both] [--drop on|off] [--audio off|out|both]
                inside a workspace a command starts in the matching directory
                pvm app catalog | pvm app install [dev1] <id>... [--yes]: install built-in catalog apps in the vm
  default-vm    print, set or clear the default vm: pvm default-vm [dev1|--clear]
  doctor        diagnose local state consistency and permissions (read-only, redacted)
  audit         read-only health audit: pvm audit [dev1] | pvm audit dev1 --privacy (vm, route, tcp/udp/icmp probes, dns, ipv6, timezone, locale)

default vm:
  commands shown with [dev1] may leave the vm out. pvm then uses the vm of the
  workspace that contains the current directory, else the one set with
  pvm default-vm, else the only user vm, and prints which one on stderr. a
  default vm that was deleted is an error until it is changed or cleared;
  pvm rm clears it. stop, rm, compact, resize, snapshot, clone and route
  always need the name. at a terminal pvm asks before starting the
  daemon or a stopped vm, and --yes starts the vm without asking. without a
  terminal the daemon still starts on its own, but a stopped vm is an error.

workspace:
  workspace create  bind a host folder to a vm at /workspace/<id>:
                    pvm workspace create web [dev1] --mode mount|sync|copy [--path DIR] [--read-only] [-y]
                    mount: one shared copy (9p; reverse sshfs on native windows)
                    sync: a copy on each side, moved by push/pull; copy: upload once, clone back
  workspace ls      list workspaces
  workspace status|push|pull  sync mode: pvm workspace push web [--prefer host|vm | --keep-both]
  workspace clone   download the vm copy into a new folder: pvm workspace clone web ./web-result
  workspace ssh     ssh into the workspace directory: pvm workspace ssh web [cmd...]
  workspace rm      forget a workspace (host files kept): pvm workspace rm web [--delete-guest] [-y]

network:
  map           map ports: pvm map [dev1] 8080:80 [--udp] | ls | rm <port> [--udp]
  route         vm default route: pvm route dev1 --via my-exit|direct
                split rules: pvm route add direct|proxy|internal [--domain-suffix|--domain-prefix|--domain-keyword|--cidr] <v> [--via <exit>] [--dns <srv>]
                inspect: pvm route ls | pvm route explain dev1 example.com | pvm route rm <index>
  dns           inspect fake-ip mappings: pvm dns ls
  exit          manage exit nodes: add (--transport auto=probe&pick fastest) | add-warp | set | ls | rm | test [--speed]
                add cloudflare warp: pvm exit add-warp my-warp
                export a standard wireguard client: pvm exit client-config my-exit [-o phone.conf]
  exit-agent    run the vps exit agent: pvm exit-agent --listen :18443 --token-file <private-file>

image:
  image fetch   download a base image (ubuntu:26.04, debian:13)
  image build   bake a golden image (docker + firefox + xfce desktop preinstalled; ubuntu/debian only)
                flags: --keep-vm --cpus N --mem MB --disk GB
  image list    list base and golden images

daemon:
  daemon start|stop|status|restart
  daemon run                foreground (used by launchd/systemd)
  daemon install|uninstall  register as a login service

other:
  version       print version
  skill         print the embedded pvm skill doc (install into agent skills)
  security      print the embedded security boundaries and verification guide
  upgrade       self-upgrade and restart the daemon: pvm upgrade [--check] [--to <tag>] [--no-restart] [-y] | pvm upgrade --rollback [-y]

common choices:
  images        ubuntu:26.04 | ubuntu:24.04 | debian:13 | debian:12
  transports    udp (default, wireguard) | tcp (wireguard over tcp; pvm exits only)
  providers     pvm exit-agent | cloudflare warp (udp only)
  nat backends  auto | nft | iptables | userspace (no root; icmp via ping socket)
  route via     direct | <exit-name>  (for example: my-exit)
  backend       auto | native | wsl (windows only; PVM_BACKEND or config.yaml backend)

create options:
  --image IMG   ubuntu:26.04 | ubuntu:24.04 | debian:13 | debian:12
  --distro D --version V  same as --image D:V (conflict with --image errors)
  --name NAME   vm name (required)
  --user USER   guest login user (required for non-interactive use)
  --cpus N      vcpus (default: quarter of host logical cpus, rounded up)
  --mem MB      memory in mb (default: quarter of host ram; idle pages return via balloon)
  --disk GB     disk size in gb (default 20)
  --tz TZ       iana timezone, e.g. America/Los_Angeles
  --locale LOC  guest locale (default en_US.UTF-8)
  --git-email E optional neutral git identity (never inherited from host)
  --git-name N  optional neutral git name (requires --git-email)
  --smbios-spoof  spoof dmi as a dell optiplex desktop
  --share MODE  private (default), global, or none
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
	if delegate, err := cli.ShouldDelegateToWSL(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "pvm: %v\n", err)
		os.Exit(1)
	} else if delegate {
		os.Exit(cli.DelegateToWSL(os.Args[1:]))
	}
	switch os.Args[1] {
	case "audit", "doctor", "route", "upgrade", "version", "--version", "-v", "skill", "security", "exit-agent", "agent", "gateway", "__snapshot-pipe-out", "__snapshot-pipe-in", "__probe-rtt":
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
	case "compact":
		code = cli.CmdCompact(args)
	case "workspace":
		code = cli.CmdWorkspace(args)
	case "mount":
		code = cli.CmdMount(args)
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
	case "__probe-rtt":
		code = cli.CmdProbeRTT(args)
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
	case "app":
		code = cli.CmdApp(args)
	case "audit":
		code = cli.CmdAudit(args)
	case "default-vm":
		code = cli.CmdDefaultVM(args)
	case "doctor":
		code = cli.CmdDoctor(args)
	case "gateway":
		fmt.Println("gateway is managed automatically by the pvm daemon; no manual action is required")
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		fmt.Printf(usage, buildinfo.Version)
		os.Exit(2)
	}
	os.Exit(code)
}
