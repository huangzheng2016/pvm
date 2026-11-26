package cli

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/huangzheng2016/pvm/internal/daemon"
	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/netcheck"
	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/tunnel"
	"github.com/huangzheng2016/pvm/internal/vm"
)

func CmdExitAgent(args []string) int {
	if len(args) > 0 && (args[0] == "install" || args[0] == "uninstall") {
		return cmdExitAgentService(args[0], args[1:])
	}
	fs := newFlagSet("exit-agent", "install|uninstall | --token <tok> [--listen :18443] [--transport both|udp|tcp]")
	listen := fs.String("listen", ":18443", "control plane TLS listen address")
	token := fs.String("token", "", "bearer token (prefer --token-file to avoid process arguments)")
	tokenFile := fs.String("token-file", "", "private file containing the control token")
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
	tz := fs.String("timezone", "", "suggested IANA timezone (default: local system timezone; no remote lookup)")
	dcAllow := fs.String("domain-allow-cidr", "", "comma-separated private/link-local CIDRs allowed for exit traffic (e.g. 10.0.0.0/8)")
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
	value, err := loadToken(*token, *tokenFile)
	if err != nil {
		return fatalf("%v", err)
	}
	*token = value
	if *token == "" {
		return fatalf("--token or --token-file is required")
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
		SuggestedTZ:    *tz,
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
		fmt.Fprintln(os.Stderr, "usage: pvm exit <add|add-warp|set|ls|rm|test|client-config> [args]")
		return 2
	}
	switch args[0] {
	case "import":
		return exitImport(args[1:])
	case "nodes":
		return exitNodes(args[1:])
	case "add":
		return exitAdd(args[1:])
	case "add-warp":
		return exitAddWARP(args[1:])
	case "set":
		return exitSet(args[1:])
	case "ls", "list":
		args = scanJSONFlag(args[1:])
		if len(args) != 0 {
			return 2
		}
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
	token := fs.String("token", "", "control plane token (prefer --token-file)")
	tokenFile := fs.String("token-file", "", "private file containing the control token")
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
	value, err := loadToken(*token, *tokenFile)
	if err != nil {
		return fatalf("%v", err)
	}
	*token = value
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
	fs := newFlagSet("exit set", "<name> [--tz TZ] [--locale L] [--ip-mode static|dynamic|off] [--ip-scope city|region|country|any] [--rebind]")
	tz := fs.String("tz", "", "IANA timezone of this exit's region")
	locale := fs.String("locale", "", "locale matching this exit's region")
	ipMode := fs.String("ip-mode", "", "exit drift protection: static | dynamic | off")
	ipScope := fs.String("ip-scope", "", "dynamic scope: city | region | country | any")
	rebind := fs.Bool("rebind", false, "explicitly re-anchor an existing exit baseline")
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
	needPush := false
	if *ipMode != "" {
		switch *ipMode {
		case "off":
			e.Policy = nil
		case "static", "dynamic":
			pol := &netcheck.ExitPolicy{Mode: *ipMode, Scope: *ipScope}
			if pol.Scope == "" {
				pol.Scope = "country"
			}
			if *ipMode == "dynamic" {
				switch pol.Scope {
				case "city", "region", "country", "any":
				default:
					return fatalf("invalid --ip-scope %q (want city, region, country, or any)", pol.Scope)
				}
			}
			if e.Policy != nil && !*rebind {
				pol.AnchorIP, pol.Country, pol.Region, pol.City = e.Policy.AnchorIP, e.Policy.Country, e.Policy.Region, e.Policy.City
			}
			if pol.AnchorIP == "" && pol.Country == "" {
				p, err := mustPaths()
				if err != nil {
					return fatalf("%v", err)
				}
				c, err := daemon.Ensure(p)
				if err != nil {
					return fatalf("%v", err)
				}
				var res struct {
					IP  string            `json:"ip"`
					Geo *netcheck.GeoInfo `json:"geo"`
				}
				if err := c.Post("/v1/exits/"+url.PathEscape(name)+"/probe", nil, &res); err != nil {
					return fatalf("cannot anchor exit baseline: %v", err)
				}
				pol.AnchorIP, pol.Country = res.IP, res.Geo.CountryCode
				pol.Region, pol.City = res.Geo.Region, res.Geo.City
			}
			e.Policy = pol
		default:
			return fatalf("invalid --ip-mode %q (want static, dynamic, or off)", *ipMode)
		}
		needPush = true
	}
	if err := exit.SaveEntry(dir, e); err != nil {
		return fatalf("save entry: %v", err)
	}
	if needPush {
		if p, err := paths.Default(); err == nil {
			c := daemon.NewClient(p)
			if c.Healthy() {
				_ = c.Post("/v1/clusters/1/push", nil, nil)
			}
		}
	}
	if e.Policy != nil {
		fmt.Printf("exit %q policy: mode=%s scope=%s anchor_ip=%s country=%s region=%s city=%s\n", name, e.Policy.Mode, e.Policy.Scope, e.Policy.AnchorIP, e.Policy.Country, e.Policy.Region, e.Policy.City)
	}
	fmt.Printf("exit %q updated: tz=%s locale=%s (region applies on next `pvm route <vm> --via %s`)\n", name, e.TZ, e.Locale, name)
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
	if jsonOutput {
		type exitSafeView struct {
			Name      string `json:"name"`
			Provider  string `json:"provider"`
			Server    string `json:"server"`
			Transport string `json:"transport"`
			TunnelIP  string `json:"tunnel_ip"`
			Endpoint  string `json:"endpoint"`
			TZ        string `json:"tz"`
		}
		views := []exitSafeView{}
		for _, e := range entries {
			provider := e.Provider
			if provider == "" {
				provider = "agent"
			}
			tz := e.TZ
			if tz == "" {
				tz = e.AutoTZ
			}
			views = append(views, exitSafeView{Name: e.Name, Provider: provider, Server: e.Server, Transport: e.Transport, TunnelIP: e.TunnelIP, Endpoint: e.Endpoint, TZ: tz})
		}
		return failErr(writeJSONOut(map[string]any{"status": "completed", "data": views}))
	}
	if len(entries) == 0 {
		fmt.Println("no exit nodes")
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tTYPE\tSERVER\tTRANSPORT\tTUNNEL IP\tENDPOINT\tTZ")
	for _, e := range entries {
		tz := "-"
		if e.TZ != "" {
			tz = e.TZ
		} else if e.AutoTZ != "" {
			tz = e.AutoTZ + "(auto)"
		}
		provider := e.Provider
		if provider == "" {
			provider = "agent"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.Name, provider, e.Server, e.Transport, e.TunnelIP, e.Endpoint, tz)
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
	if e.IsWARP() {
		if err := deleteWARPRegistration(e); err != nil {
			fmt.Fprintf(os.Stderr, "warning: delete Cloudflare device failed (continuing): %v\n", err)
		}
	} else if e.IsMihomo() {
		if p, err := paths.Default(); err == nil {
			if err := removeMihomoExitIfNeeded(daemon.NewClient(p), e); err != nil {
				fmt.Fprintf(os.Stderr, "warning: gateway mihomo cleanup incomplete (continuing): %v\n", err)
			}
		}
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := exit.NewClient(e.Server, e.Token, e.Fingerprint).DeletePeer(ctx, e.TunnelIP); err != nil {
			fmt.Fprintf(os.Stderr, "warning: revoke on server failed (continuing): %v\n", err)
		}
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
	if e.IsWARP() && *speed {
		return fatalf("--speed is available only for pvm exit-agent nodes")
	}
	fmt.Printf("dialing %s (%s %s)...\n", e.Name, e.Transport, e.Endpoint)
	if e.ServerIP == "" && !e.IsWARP() {
		if err := reEnrollExit(dir, e); err != nil {
			return fatalf("re-enroll: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	probePublicEgress := func() {
		p, err := paths.Default()
		if err != nil {
			return
		}
		c := daemon.NewClient(p)
		if !c.Healthy() {
			fmt.Println("egress probe: skipped (daemon not running; start a cluster gateway for real internet egress verification)")
			return
		}
		var res struct {
			IP      string            `json:"ip"`
			Service string            `json:"service"`
			Geo     *netcheck.GeoInfo `json:"geo"`
		}
		if err := c.Post("/v1/exits/"+url.PathEscape(name)+"/probe", nil, &res); err != nil {
			fmt.Printf("egress=fail: real internet egress through %s failed: %v (check VPS firewall forwarding, e.g. ufw route allow)\n", name, err)
			return
		}
		loc := ""
		if res.Geo != nil {
			loc = fmt.Sprintf(" country=%s tz=%s", res.Geo.CountryCode, res.Geo.Timezone)
		}
		fmt.Printf("egress=ok: public exit ip=%s via %s%s\n", res.IP, res.Service, loc)
	}
	dial := func() (*exit.Tunnel, error) {
		t, err := exit.DialEntry(e)
		if err != nil {
			return nil, err
		}
		target := e.ServerIP
		if e.IsWARP() {
			// Any packet forces the handshake; TEST-NET-3 keeps the
			// trigger off real third-party infrastructure.
			target = "203.0.113.1"
		}
		if err := t.WaitHandshake(ctx, target); err != nil {
			t.Close()
			return nil, err
		}
		return t, nil
	}
	t, err := dial()
	if err != nil {
		if e.IsWARP() {
			return fatalf("handshake: %v", err)
		}
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
	if e.IsWARP() {
		fmt.Println("own-agent probes: skipped (WARP exits have no pvm exit-agent to probe)")
		probePublicEgress()
		return 0
	}
	sip := netip.MustParseAddr(serverIP)
	if _, err := t.Ping(ctx, sip); err != nil {
		return fatalf("tcp /ping: %v", err)
	}
	fmt.Println("tcp /ping: ok")
	if err := t.ProbeUDP(ctx, sip); err != nil {
		return fatalf("udp probe: %v", err)
	}
	fmt.Println("udp probe: ok")
	if err := t.ProbeICMP(ctx, sip); err != nil {
		return fatalf("icmp probe: %v", err)
	}
	fmt.Println("icmp probe: ok")
	if *speed {
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
	probePublicEgress()
	return 0
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
