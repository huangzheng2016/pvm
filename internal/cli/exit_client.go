package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/tunnel"
	"github.com/mdp/qrterminal/v3"
)

func exitClientConfig(args []string) int {
	return exitClientConfigTo(args, os.Stdout)
}

func exitClientConfigTo(args []string, stdout io.Writer) int {
	fs := newFlagSet("exit client-config", "<name> [--output file]")
	var output string
	fs.StringVar(&output, "output", "", "config path (default: <name>-client.conf; must not exist)")
	fs.StringVar(&output, "o", "", "config path (shorthand)")
	name := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name = args[0]
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		if name != "" || fs.NArg() > 1 {
			fs.Usage()
			return 2
		}
		name = fs.Arg(0)
	}
	if name == "" {
		fs.Usage()
		return 2
	}
	if output == "" {
		output = name + "-client.conf"
	}
	if _, err := os.Lstat(output); err == nil {
		return fatalf("%s already exists; choose another --output", output)
	} else if !os.IsNotExist(err) {
		return fatalf("check output: %v", err)
	}

	dir, err := exitsDir()
	if err != nil {
		return fatalf("%v", err)
	}
	e, err := exit.LoadEntry(dir, name)
	if err != nil {
		return fatalf("exit %q not found", name)
	}
	if e.Transport != "udp" {
		return fatalf("standard WireGuard clients require UDP; exit %q uses %s", name, e.Transport)
	}

	priv, err := tunnel.GenerateKey()
	if err != nil {
		return fatalf("generate client key: %v", err)
	}
	pub, err := tunnel.PublicKey(priv)
	if err != nil {
		return fatalf("generate client key: %v", err)
	}
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "pvm-client"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := exit.NewClient(e.Server, e.Token, e.Fingerprint)
	resp, err := client.Enroll(ctx, pub, hostname+"-client", "udp")
	if err != nil {
		return fatalf("enroll client: %v", err)
	}
	keepPeer := false
	defer func() {
		if keepPeer {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := client.DeletePeer(ctx, resp.TunnelIP); err != nil {
			fmt.Fprintf(os.Stderr, "warning: revoke unused client peer: %v\n", err)
		}
	}()

	conf, err := standardWGConfig(priv, resp.TunnelIP, resp.ServerPubKey, resp.Endpoint, resp.MTU)
	if err != nil {
		return fatalf("invalid enrollment response: %v", err)
	}
	if err := writeNewPrivateFile(output, []byte(conf)); err != nil {
		return fatalf("write config: %v", err)
	}
	keepPeer = true

	abs, err := filepath.Abs(output)
	if err != nil {
		abs = output
	}
	fmt.Fprintf(stdout, "wrote %s (mode 0600)\n", abs)
	fmt.Fprintln(stdout, "scan with a standard WireGuard client; the QR code contains the client private key")
	qrterminal.GenerateHalfBlock(conf, qrterminal.L, stdout)
	return 0
}

func standardWGConfig(privateKey, tunnelIP, serverPubKey, endpoint string, mtu int) (string, error) {
	ip, err := netip.ParseAddr(tunnelIP)
	if err != nil || !ip.Is4() {
		return "", fmt.Errorf("invalid IPv4 tunnel address %q", tunnelIP)
	}
	if _, err := tunnel.Hex(serverPubKey); err != nil {
		return "", fmt.Errorf("invalid server public key")
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil || host == "" || strings.ContainsAny(host, " \t\r\n") {
		return "", fmt.Errorf("invalid endpoint %q", endpoint)
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil || p == 0 {
		return "", fmt.Errorf("invalid endpoint %q", endpoint)
	}
	if mtu < 576 || mtu > 65535 {
		return "", fmt.Errorf("invalid MTU %d", mtu)
	}
	return fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = %s/32
DNS = 1.1.1.1
MTU = %d

[Peer]
PublicKey = %s
Endpoint = %s
AllowedIPs = 0.0.0.0/0
PersistentKeepalive = 25
`, privateKey, ip, mtu, serverPubKey, endpoint), nil
}

func writeNewPrivateFile(path string, data []byte) (err error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(path)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	return f.Close()
}
