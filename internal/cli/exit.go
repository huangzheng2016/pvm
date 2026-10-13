package cli

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/tunnel"
	"github.com/huangzheng2016/pvm/internal/vm"
)

func CmdExitAgent(args []string) int {
	fs := newFlagSet("exit-agent", "--token <tok> [--listen :18443] [--transport both|udp|tcp]")
	listen := fs.String("listen", ":18443", "control plane TLS listen address")
	token := fs.String("token", "", "bearer token for the control plane (required)")
	state := fs.String("state", "/var/lib/pvm-exit", "state directory (cert, wg key, peers)")
	pub := fs.String("public-endpoint", "", "host[:port] announced to clients (default: derive from request)")
	transports := fs.String("transport", "both", "udp | tcp | both")
	nat := fs.String("nat", "auto", "nat backend: auto | nft | iptables | userspace")
	tunPrefix := fs.String("tun-prefix", "pvmexit", "OS TUN name prefix (change when running multiple agents)")
	subnetBase := fs.Int("subnet-base", 99, "tunnel pools (1-254): udp 10.<base>.0.0/16, tcp 10.<base+1>.0.0/16")
	status := fs.Bool("status", false, "print the effective nat backend and exit")
	udpPort := fs.Int("udp-port", 18443, "wireguard UDP port")
	tcpPort := fs.Int("tcp-port", 18444, "wireguard TCP port")
	verbose := fs.Bool("v", false, "verbose wireguard logging")
	dcAllow := fs.String("domain-allow-cidr", "", "comma-separated CIDRs the domain-connect service may dial despite being private/link-local (e.g. 10.0.0.0/8)")
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
		DomainAllow:    splitCSV(*dcAllow),
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

func CmdExit(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: pvm exit <add|set|ls|rm|test|client-config> [args]")
		return 2
	}
	switch args[0] {
	case "add":
		return exitAdd(args[1:])
	case "set":
		return exitSet(args[1:])
	case "ls", "list":
		return exitLs()
	case "rm", "del":
		return exitRm(args[1:])
	case "test":
		return exitTest(args[1:])
	case "client-config":
		return exitClientConfig(args[1:])
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
	fs := newFlagSet("exit add", "<name> --server host:port --token <tok> [--transport auto|udp|tcp] [--tz TZ] [--locale L]")
	server := fs.String("server", "", "control plane host:port")
	token := fs.String("token", "", "control plane token")
	transport := fs.String("transport", "auto", "auto | udp | tcp (auto probes both and picks the fastest)")
	samples := fs.Int("samples", 3, "speed samples per transport when probing (averaged)")
	fpExpect := fs.String("fingerprint", "", "expected server cert fingerprint (out-of-band; aborts on mismatch instead of blind TOFU)")
	tz := fs.String("tz", "", "IANA timezone of this exit's region (e.g. America/Los_Angeles)")
	locale := fs.String("locale", "", "locale matching this exit's region (e.g. en_US.UTF-8)")
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
	if *transport != "auto" && *transport != "udp" && *transport != "tcp" {
		return fatalf("transport must be auto, udp or tcp")
	}
	if *tz != "" {
		if err := vm.ValidateTZ(*tz); err != nil {
			return fatalf("%v", err)
		}
	}
	if *locale != "" {
		l, err := vm.NormalizeLocale(*locale)
		if err != nil {
			return fatalf("%v", err)
		}
		*locale = l
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
	if *fpExpect != "" && !strings.EqualFold(*fpExpect, fp) {
		return fatalf("fingerprint mismatch: got %s, want %s (possible MITM; aborting before sending the token)", fp, *fpExpect)
	}
	fmt.Printf("server cert fingerprint: %s (pinned)\n", fp)

	var e *exit.Entry
	var suggestedTZ string
	if *transport == "auto" {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		fmt.Printf("probing transports in parallel (%d x 8MB download each)...\n", *samples)
		probes := exit.ProbeAll(ctx, *server, *token, fp, []string{"udp", "tcp"}, exit.ProbeSpeedBytes, *samples)
		for _, pr := range probes {
			if pr.Err != nil {
				fmt.Printf("  %-4s failed: %v\n", pr.Transport, pr.Err)
				continue
			}
			for i, s := range pr.Samples {
				if s.Err != nil {
					fmt.Printf("  %-4s sample%d failed: %v\n", pr.Transport, i+1, s.Err)
				} else {
					fmt.Printf("  %-4s sample%d %.1f MB/s  rtt %s\n", pr.Transport, i+1, s.Mbps, s.RTT.Round(time.Millisecond))
				}
			}
			fmt.Printf("  %-4s avg %.1f MB/s  rtt %s\n", pr.Transport, pr.Mbps(), pr.RTT.Round(time.Millisecond))
		}
		winner, err := exit.ChooseTransport(probes)
		if err != nil {
			for _, pr := range probes {
				exit.RevokeProbe(ctx, *server, *token, fp, pr)
			}
			return fatalf("%v", err)
		}
		for _, pr := range probes {
			if pr != winner {
				exit.RevokeProbe(ctx, *server, *token, fp, pr)
			}
		}
		fmt.Printf("selected transport: %s\n", winner.Transport)
		e = winner.Entry(name, *server, *token, fp)
		suggestedTZ = winner.Resp.SuggestedTZ
	} else {
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
		e = &exit.Entry{
			Name:         name,
			Server:       *server,
			Token:        *token,
			Transport:    *transport,
			Fingerprint:  fp,
			WGPrivateKey: priv,
			WGPublicKey:  pub,
			ServerPubKey: resp.ServerPubKey,
			TunnelIP:     resp.TunnelIP,
			ServerIP:     resp.ServerIP,
			MTU:          resp.MTU,
			Endpoint:     resp.Endpoint,
			CreatedAt:    time.Now(),
		}
		suggestedTZ = resp.SuggestedTZ
	}
	e.TZ = *tz
	e.Locale = *locale
	if *tz == "" {
		e.AutoTZ = suggestedTZ
	}
	if err := exit.SaveEntry(dir, e); err != nil {
		return fatalf("save entry: %v", err)
	}
	fmt.Printf("exit %q added: transport=%s tunnel_ip=%s endpoint=%s mtu=%d\n",
		name, e.Transport, e.TunnelIP, e.Endpoint, e.MTU)
	if e.AutoTZ != "" {
		fmt.Printf("suggested timezone from server: %s (auto; override with `pvm exit set %s --tz ...`)\n", e.AutoTZ, name)
	}
	return 0
}

func exitSet(args []string) int {
	fs := newFlagSet("exit set", "<name> [--tz TZ] [--locale L]")
	tz := fs.String("tz", "", "IANA timezone of this exit's region")
	locale := fs.String("locale", "", "locale matching this exit's region")
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
	if name == "" {
		fs.Usage()
		return 2
	}
	if *tz != "" {
		if err := vm.ValidateTZ(*tz); err != nil {
			return fatalf("%v", err)
		}
	}
	if *locale != "" {
		l, err := vm.NormalizeLocale(*locale)
		if err != nil {
			return fatalf("%v", err)
		}
		*locale = l
	}
	dir, err := exitsDir()
	if err != nil {
		return fatalf("%v", err)
	}
	e, err := exit.LoadEntry(dir, name)
	if err != nil {
		return fatalf("exit %q not found", name)
	}
	if *tz != "" {
		e.TZ = *tz
	}
	if *locale != "" {
		e.Locale = *locale
	}
	if err := exit.SaveEntry(dir, e); err != nil {
		return fatalf("save entry: %v", err)
	}
	fmt.Printf("exit %q updated: tz=%s locale=%s (applies on next `pvm route <vm> --via %s`)\n", name, e.TZ, e.Locale, name)
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
	fmt.Fprintln(tw, "NAME\tSERVER\tTRANSPORT\tTUNNEL IP\tENDPOINT\tTZ")
	for _, e := range entries {
		tz := "-"
		if e.TZ != "" {
			tz = e.TZ
		} else if e.AutoTZ != "" {
			tz = e.AutoTZ + "(auto)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", e.Name, e.Server, e.Transport, e.TunnelIP, e.Endpoint, tz)
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
	fs := newFlagSet("exit test", "<name> [--speed]")
	speed := fs.Bool("speed", false, "also measure tunneled download throughput and RTT")
	samples := fs.Int("samples", 3, "speed samples (averaged)")
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name = args[0]
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	if name == "" {
		fs.Usage()
		return 2
	}
	dir, err := exitsDir()
	if err != nil {
		return fatalf("%v", err)
	}
	e, err := exit.LoadEntry(dir, name)
	if err != nil {
		return fatalf("exit %q not found", name)
	}
	fmt.Printf("dialing %s (%s %s)...\n", e.Name, e.Transport, e.Endpoint)
	if e.ServerIP == "" {
		// Handshake verification needs ServerIP, which older saved entries may lack.
		if err := reEnrollExit(dir, e); err != nil {
			return fatalf("re-enroll: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dial := func() (*exit.Tunnel, error) {
		t, err := exit.DialEntry(e)
		if err != nil {
			return nil, err
		}
		if err := t.WaitHandshake(ctx, e.ServerIP); err != nil {
			t.Close()
			return nil, err
		}
		return t, nil
	}
	t, err := dial()
	if err != nil {
		// The server reclaims idle peers and reassigns freed tunnel IPs,
		// so a stored entry can go stale; re-enroll once and retry.
		fmt.Println("handshake failed; re-enrolling peer...")
		if rerr := reEnrollExit(dir, e); rerr != nil {
			return fatalf("handshake: %v (re-enroll: %v)", err, rerr)
		}
		if t, err = dial(); err != nil {
			return fatalf("handshake after re-enroll: %v", err)
		}
	}
	defer t.Close()
	serverIP := e.ServerIP
	fmt.Println("wireguard handshake: ok")
	if *speed {
		sip := netip.MustParseAddr(serverIP)
		results := t.SampleSpeed(ctx, sip, exit.ProbeSpeedBytes, *samples)
		for i, s := range results {
			if s.Err != nil {
				fmt.Printf("sample%d failed: %v\n", i+1, s.Err)
			} else {
				fmt.Printf("sample%d %.1f MB/s  rtt %s\n", i+1, s.Mbps, s.RTT.Round(time.Millisecond))
			}
		}
		rtt, mbps, err := exit.AggregateSamples(results)
		if err != nil {
			return fatalf("speed: %v", err)
		}
		fmt.Printf("avg: %.1f MB/s, rtt %s\n", mbps, rtt.Round(time.Millisecond))
	}
	ip, err := t.FetchExitIP(ctx)
	if err != nil {
		return fatalf("fetch exit ip: %v", err)
	}
	fmt.Printf("exit ip observed through tunnel: %s\n", ip)

	host, _, err := net.SplitHostPort(e.Server)
	if err != nil {
		host = e.Server
	}
	matches, err := observedIPMatchesHost(host, ip, func(h string) ([]net.IP, error) {
		return net.DefaultResolver.LookupIP(ctx, "ip", h)
	})
	if err != nil {
		return fatalf("resolve server %s: %v", host, err)
	}
	if matches {
		fmt.Println("matches server address: traffic exits via the VPS")
		return 0
	}
	return fatalf("MISMATCH: exit ip %s != server %s", ip, host)
}

type hostIPResolver func(string) ([]net.IP, error)

func reEnrollExit(dir string, e *exit.Entry) error {
	host, err := os.Hostname()
	if err != nil {
		host = "pvm"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := exit.NewClient(e.Server, e.Token, e.Fingerprint)
	resp, err := client.Enroll(ctx, e.WGPublicKey, host, e.Transport)
	if err != nil {
		return err
	}
	e.ServerPubKey = resp.ServerPubKey
	e.TunnelIP = resp.TunnelIP
	e.ServerIP = resp.ServerIP
	e.MTU = resp.MTU
	e.Endpoint = resp.Endpoint
	return exit.SaveEntry(dir, e)
}

func observedIPMatchesHost(host, observed string, resolve hostIPResolver) (bool, error) {
	observedIP := net.ParseIP(observed)
	if observedIP == nil {
		return false, fmt.Errorf("invalid observed ip %q", observed)
	}
	if hostIP := net.ParseIP(host); hostIP != nil {
		return hostIP.Equal(observedIP), nil
	}
	ips, err := resolve(host)
	if err != nil {
		return false, err
	}
	for _, ip := range ips {
		if ip.Equal(observedIP) {
			return true, nil
		}
	}
	return false, nil
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
