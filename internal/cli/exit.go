package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/tunnel"
)

// CmdExitAgent runs the VPS-side exit agent in the foreground.
func CmdExitAgent(args []string) int {
	fs := newFlagSet("exit-agent", "--token <tok> [--listen :18443] [--transport both|udp|tcp]")
	listen := fs.String("listen", ":18443", "control plane TLS listen address")
	token := fs.String("token", "", "bearer token for the control plane (required)")
	state := fs.String("state", "/var/lib/pvm-exit", "state directory (cert, wg key, peers)")
	pub := fs.String("public-endpoint", "", "host[:port] announced to clients (default: derive from request)")
	transports := fs.String("transport", "both", "udp | tcp | both")
	nat := fs.String("nat", "auto", "nat backend: auto | nft | iptables | userspace")
	tunPrefix := fs.String("tun-prefix", "pvmexit", "OS TUN name prefix (change when running multiple agents)")
	subnetBase := fs.Int("subnet-base", 99, "tunnel subnet octet: 10.<base>.0/1.0/24 (change when running multiple agents)")
	status := fs.Bool("status", false, "print the effective nat backend and exit")
	udpPort := fs.Int("udp-port", 18443, "wireguard UDP port")
	tcpPort := fs.Int("tcp-port", 18444, "wireguard TCP port")
	verbose := fs.Bool("v", false, "verbose wireguard logging")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *status {
		b, err := exit.ResolveNATName(*nat)
		if err != nil {
			return fatalf("%v", err)
		}
		fmt.Println(b)
		return 0
	}
	if *token == "" {
		return fatalf("--token is required")
	}
	srv, err := exit.NewServer(exit.ServerConfig{
		Listen:         *listen,
		Token:          *token,
		StateDir:       *state,
		PublicEndpoint: *pub,
		UDPPort:        *udpPort,
		TCPPort:        *tcpPort,
		Transports:     *transports,
		NAT:            *nat,
		TunPrefix:      *tunPrefix,
		SubnetBase:     *subnetBase,
		Verbose:        *verbose,
	})
	if err != nil {
		return fatalf("exit-agent: %v", err)
	}
	fp, err := srv.Start()
	if err != nil {
		return fatalf("exit-agent: %v", err)
	}
	fmt.Printf("pvm exit-agent listening %s\n", *listen)
	fmt.Printf("cert fingerprint (TOFU pin this): %s\n", fp)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	fmt.Println("\nshutting down")
	srv.Close()
	return 0
}

// CmdExit manages local exit-node entries.
func CmdExit(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: pvm exit <add|ls|rm|test> [args]")
		return 2
	}
	switch args[0] {
	case "add":
		return exitAdd(args[1:])
	case "ls", "list":
		return exitLs()
	case "rm", "del":
		return exitRm(args[1:])
	case "test":
		return exitTest(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown exit subcommand %q\n", args[0])
		return 2
	}
}

func exitsDir() (string, error) {
	p, err := mustPaths()
	if err != nil {
		return "", err
	}
	return filepath.Join(p.Home, "exits"), nil
}

func exitAdd(args []string) int {
	fs := newFlagSet("exit add", "<name> --server host:port --token <tok> [--transport udp|tcp]")
	server := fs.String("server", "", "control plane host:port")
	token := fs.String("token", "", "control plane token")
	transport := fs.String("transport", "udp", "udp | tcp")
	// accept "add <name> --flags" even though flag stops at the first
	// positional argument
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name = args[0]
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		if name != "" {
			fs.Usage()
			return 2
		}
		name = fs.Arg(0)
	}
	if name == "" || *server == "" || *token == "" {
		fs.Usage()
		return 2
	}
	if *transport != "udp" && *transport != "tcp" {
		return fatalf("transport must be udp or tcp")
	}
	dir, err := exitsDir()
	if err != nil {
		return fatalf("%v", err)
	}
	if _, err := exit.LoadEntry(dir, name); err == nil {
		return fatalf("exit %q already exists", name)
	}

	// TOFU: capture the server cert fingerprint on first contact, then pin it.
	fp, err := exit.ProbeFingerprint(*server)
	if err != nil {
		return fatalf("probe %s: %v", *server, err)
	}
	fmt.Printf("server cert fingerprint: %s (pinned)\n", fp)

	priv, err := tunnel.GenerateKey()
	if err != nil {
		return fatalf("%v", err)
	}
	pub, err := tunnel.PublicKey(priv)
	if err != nil {
		return fatalf("%v", err)
	}
	host, err := os.Hostname()
	if err != nil {
		host = "pvm"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := exit.NewClient(*server, *token, fp)
	resp, err := client.Enroll(ctx, pub, host, *transport)
	if err != nil {
		return fatalf("enroll: %v", err)
	}
	e := &exit.Entry{
		Name:         name,
		Server:       *server,
		Token:        *token,
		Transport:    *transport,
		Fingerprint:  fp,
		WGPrivateKey: priv,
		WGPublicKey:  pub,
		ServerPubKey: resp.ServerPubKey,
		TunnelIP:     resp.TunnelIP,
		MTU:          resp.MTU,
		Endpoint:     resp.Endpoint,
		CreatedAt:    time.Now(),
	}
	if err := exit.SaveEntry(dir, e); err != nil {
		return fatalf("save entry: %v", err)
	}
	fmt.Printf("exit %q added: transport=%s tunnel_ip=%s endpoint=%s mtu=%d\n",
		name, e.Transport, e.TunnelIP, e.Endpoint, e.MTU)
	return 0
}

func exitLs() int {
	dir, err := exitsDir()
	if err != nil {
		return fatalf("%v", err)
	}
	entries, err := exit.ListEntries(dir)
	if err != nil {
		return fatalf("%v", err)
	}
	if len(entries) == 0 {
		fmt.Println("no exit nodes")
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSERVER\tTRANSPORT\tTUNNEL IP\tENDPOINT")
	for _, e := range entries {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.Name, e.Server, e.Transport, e.TunnelIP, e.Endpoint)
	}
	tw.Flush()
	return 0
}

func exitRm(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: pvm exit rm <name>")
		return 2
	}
	dir, err := exitsDir()
	if err != nil {
		return fatalf("%v", err)
	}
	e, err := exit.LoadEntry(dir, args[0])
	if err != nil {
		return fatalf("exit %q not found", args[0])
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := exit.NewClient(e.Server, e.Token, e.Fingerprint).DeletePeer(ctx, e.TunnelIP); err != nil {
		fmt.Fprintf(os.Stderr, "warning: revoke on server failed (continuing): %v\n", err)
	}
	if err := exit.RemoveEntry(dir, args[0]); err != nil {
		return fatalf("%v", err)
	}
	fmt.Printf("exit %q removed\n", args[0])
	return 0
}

func exitTest(args []string) int {
	fs := newFlagSet("exit test", "<name>")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	dir, err := exitsDir()
	if err != nil {
		return fatalf("%v", err)
	}
	e, err := exit.LoadEntry(dir, fs.Arg(0))
	if err != nil {
		return fatalf("exit %q not found", fs.Arg(0))
	}
	fmt.Printf("dialing %s (%s %s)...\n", e.Name, e.Transport, e.Endpoint)
	t, err := exit.DialEntry(e)
	if err != nil {
		return fatalf("tunnel: %v", err)
	}
	defer t.Close()
	serverIP, err := exit.ServerIP(e.Transport)
	if err != nil {
		return fatalf("%v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := t.WaitHandshake(ctx, serverIP); err != nil {
		return fatalf("handshake: %v", err)
	}
	fmt.Println("wireguard handshake: ok")
	ip, err := t.FetchExitIP(ctx)
	if err != nil {
		return fatalf("fetch exit ip: %v", err)
	}
	fmt.Printf("exit ip observed through tunnel: %s\n", ip)

	host, _, err := net.SplitHostPort(e.Server)
	if err != nil {
		host = e.Server
	}
	if ip == host {
		fmt.Println("matches server address: traffic exits via the VPS")
		return 0
	}
	return fatalf("MISMATCH: exit ip %s != server %s", ip, host)
}
