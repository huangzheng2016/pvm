package cli

import (
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/huangzheng2016/pvm/internal/fsutil"
)

const exitAgentUnitPath = "/etc/systemd/system/pvm-exit-agent.service"

func cmdExitAgentService(action string, args []string) int {
	if runtime.GOOS != "linux" {
		return failErr(fmt.Errorf("exit-agent %s installs a systemd unit and is supported on Linux VPS hosts only", action))
	}
	if action == "uninstall" {
		if len(args) != 0 {
			return failErr(fmt.Errorf("usage: pvm exit-agent uninstall"))
		}
		_, _ = exec.Command("systemctl", "disable", "--now", "pvm-exit-agent.service").CombinedOutput()
		if err := os.Remove(exitAgentUnitPath); err != nil && !os.IsNotExist(err) {
			return failErr(fmt.Errorf("remove unit: %w", err))
		}
		if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
			return failErr(fmt.Errorf("systemctl daemon-reload: %v: %s", err, strings.TrimSpace(string(out))))
		}
		fmt.Println("pvm-exit-agent.service removed; token, certificate and peer state were kept")
		return 0
	}

	fs := flag.NewFlagSet("exit-agent install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	listen := fs.String("listen", ":18443", "control plane TLS listen address")
	tokenFile := fs.String("token-file", "/etc/pvm-exit/token", "existing private token file (never overwritten)")
	state := fs.String("state", "/var/lib/pvm-exit", "state directory (cert, wg key, peers)")
	pub := fs.String("public-endpoint", "", "host[:port] announced to clients")
	transports := fs.String("transport", "both", "udp | tcp | both")
	nat := fs.String("nat", "auto", "auto | nft | iptables | userspace")
	tunPrefix := fs.String("tun-prefix", "pvmexit", "OS TUN name prefix")
	subnetBase := fs.Int("subnet-base", 99, "tunnel pool base (1-254)")
	udpPort := fs.Int("udp-port", 18443, "wireguard UDP port")
	tcpPort := fs.Int("tcp-port", 18444, "wireguard TCP port")
	tz := fs.String("timezone", "", "suggested IANA timezone")
	dcAllow := fs.String("domain-allow-cidr", "", "comma-separated private CIDRs allowed")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return failErr(fmt.Errorf("usage: pvm exit-agent install [--listen :18443] [--token-file /etc/pvm-exit/token] [--state /var/lib/pvm-exit] [exit-agent flags]"))
	}
	if _, err := loadToken("", *tokenFile); err != nil {
		return failErr(fmt.Errorf("token file %s must already exist and be private (0600): %w", *tokenFile, err))
	}
	for _, s := range []string{*listen, *tokenFile, *state, *pub, *transports, *nat, *tunPrefix, *tz, *dcAllow} {
		if strings.ContainsAny(s, "\r\n\x00") {
			return failErr(fmt.Errorf("invalid control character in argument"))
		}
	}
	switch *transports {
	case "udp", "tcp", "both":
	default:
		return failErr(fmt.Errorf("invalid --transport %q", *transports))
	}
	exe, err := os.Executable()
	if err != nil {
		return failErr(err)
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return failErr(err)
	}

	active := exec.Command("systemctl", "is-active", "--quiet", "pvm-exit-agent.service").Run() == nil
	if !active {
		if err := checkExitAgentPortsFree(*listen, *transports, *udpPort, *tcpPort); err != nil {
			return failErr(err)
		}
	} else if out, err := exec.Command("systemctl", "stop", "pvm-exit-agent.service").CombinedOutput(); err != nil {
		return failErr(fmt.Errorf("stop existing pvm-exit-agent: %v: %s", err, strings.TrimSpace(string(out))))
	}

	unitArgs := []string{
		exe, "exit-agent",
		"--listen", *listen,
		"--token-file", *tokenFile,
		"--state", *state,
		"--transport", *transports,
		"--nat", *nat,
		"--tun-prefix", *tunPrefix,
		"--subnet-base", strconv.Itoa(*subnetBase),
		"--udp-port", strconv.Itoa(*udpPort),
		"--tcp-port", strconv.Itoa(*tcpPort),
	}
	if *pub != "" {
		unitArgs = append(unitArgs, "--public-endpoint", *pub)
	}
	if *tz != "" {
		unitArgs = append(unitArgs, "--timezone", *tz)
	}
	if *dcAllow != "" {
		unitArgs = append(unitArgs, "--domain-allow-cidr", *dcAllow)
	}
	unit := "[Unit]\nDescription=pvm exit agent\nAfter=network-online.target\nWants=network-online.target\n\n" +
		"[Service]\nType=simple\nExecStart=" + strings.Join(unitArgs, " ") + "\nRestart=always\nRestartSec=5\n\n" +
		"[Install]\nWantedBy=multi-user.target\n"
	if err := fsutil.AtomicWrite(exitAgentUnitPath, []byte(unit), 0o644); err != nil {
		return failErr(fmt.Errorf("write unit: %w", err))
	}
	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return failErr(fmt.Errorf("daemon-reload: %v: %s", err, strings.TrimSpace(string(out))))
	}
	if out, err := exec.Command("systemctl", "enable", "--now", "pvm-exit-agent.service").CombinedOutput(); err != nil {
		return failErr(fmt.Errorf("enable --now pvm-exit-agent: %v: %s; inspect `journalctl -u pvm-exit-agent -n 30 --no-pager`", err, strings.TrimSpace(string(out))))
	}
	fmt.Println("pvm-exit-agent.service installed and started")
	fmt.Println("inspect fingerprint with: journalctl -u pvm-exit-agent --since '-30s' --no-pager | grep fingerprint")
	return 0
}

func checkExitAgentPortsFree(listen, transports string, udpPort, tcpPort int) error {
	_, portStr, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("invalid --listen %q: %w", listen, err)
	}
	tcpPorts := []int{0}
	p, err := strconv.Atoi(portStr)
	if err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("invalid --listen port %q", portStr)
	}
	tcpPorts[0] = p
	if transports == "tcp" || transports == "both" {
		tcpPorts = append(tcpPorts, tcpPort)
	}
	for _, tp := range tcpPorts {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(tp)))
		if err != nil {
			return fmt.Errorf("tcp port %d is already in use by another service; choose another port or stop that service", tp)
		}
		ln.Close()
	}
	if transports == "udp" || transports == "both" {
		pc, err := net.ListenPacket("udp", net.JoinHostPort("0.0.0.0", strconv.Itoa(udpPort)))
		if err != nil {
			return fmt.Errorf("udp port %d is already in use by another service; choose another --udp-port", udpPort)
		}
		pc.Close()
	}
	return nil
}
