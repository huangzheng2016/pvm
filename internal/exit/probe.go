package exit

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/huangzheng2016/pvm/internal/tunnel"
)

// ProbeSpeedBytes is the default download size for transport probing.
const ProbeSpeedBytes = 8 << 20

// TransportProbe is the outcome of probing one transport end-to-end:
// enroll, handshake, ping RTT, and a tunneled download.
type TransportProbe struct {
	Transport string
	Priv      string
	Pub       string
	Resp      *enrollResponse
	RTT       time.Duration
	Bytes     int64
	Duration  time.Duration
	Err       error
}

// Mbps returns the measured download throughput in MB/s (10^6 bytes).
func (p *TransportProbe) Mbps() float64 {
	if p.Err != nil || p.Duration <= 0 {
		return 0
	}
	return float64(p.Bytes) / p.Duration.Seconds() / 1e6
}

// ProbeTransport enrolls with one transport and measures it. The probe
// keeps its enrolled peer — the caller either builds an Entry from it
// (winner) or revokes it (loser) via RevokeProbe.
func ProbeTransport(ctx context.Context, server, token, fingerprint, transport string, bytesN int64) *TransportProbe {
	p := &TransportProbe{Transport: transport}
	fail := func(err error) *TransportProbe {
		p.Err = err
		return p
	}
	priv, err := tunnel.GenerateKey()
	if err != nil {
		return fail(err)
	}
	pub, err := tunnel.PublicKey(priv)
	if err != nil {
		return fail(err)
	}
	p.Priv, p.Pub = priv, pub

	hostname := "pvm-probe"
	client := NewClient(server, token, fingerprint)
	resp, err := client.Enroll(ctx, pub, hostname, transport)
	if err != nil {
		return fail(fmt.Errorf("enroll: %w", err))
	}
	p.Resp = resp
	entry := &Entry{
		Server:       server,
		Token:        token,
		Transport:    transport,
		Fingerprint:  fingerprint,
		WGPrivateKey: priv,
		WGPublicKey:  pub,
		ServerPubKey: resp.ServerPubKey,
		TunnelIP:     resp.TunnelIP,
		MTU:          resp.MTU,
		Endpoint:     resp.Endpoint,
	}
	tun, err := DialEntry(entry)
	if err != nil {
		return fail(fmt.Errorf("tunnel: %w", err))
	}
	defer tun.Close()

	serverIP := resp.ServerIP
	if serverIP == "" {
		serverIP, _ = ServerIP(transport)
	}
	hctx, hcancel := context.WithTimeout(ctx, 10*time.Second)
	defer hcancel()
	if err := tun.WaitHandshake(hctx, serverIP); err != nil {
		return fail(fmt.Errorf("handshake: %w", err))
	}

	sip := netip.MustParseAddr(serverIP)
	t0 := time.Now()
	if _, err := tun.httpGet(ctx, sip, SpeedPort, serverIP, "/ping"); err != nil {
		return fail(fmt.Errorf("ping: %w", err))
	}
	p.RTT = time.Since(t0)

	n, dur, err := tun.download(ctx, sip, SpeedPort, fmt.Sprintf("/speed?bytes=%d", bytesN))
	if err != nil {
		return fail(fmt.Errorf("download: %w", err))
	}
	p.Bytes, p.Duration = n, dur
	return p
}

// ProbeAll probes every given transport in parallel.
func ProbeAll(ctx context.Context, server, token, fingerprint string, transports []string, bytesN int64) []*TransportProbe {
	out := make([]*TransportProbe, len(transports))
	var wg sync.WaitGroup
	for i, tr := range transports {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = ProbeTransport(ctx, server, token, fingerprint, tr, bytesN)
		}()
	}
	wg.Wait()
	return out
}

// RevokeProbe removes a probe's enrolled peer on the server.
func RevokeProbe(ctx context.Context, server, token, fingerprint string, p *TransportProbe) {
	if p == nil || p.Resp == nil {
		return
	}
	_ = NewClient(server, token, fingerprint).DeletePeer(ctx, p.Resp.TunnelIP)
}

// ChooseTransport picks the winning transport:
//   - UDP wins whenever usable and not slower than 80% of TCP
//     (UDP avoids TCP-over-TCP pathologies);
//   - a single usable transport wins by default;
//   - none usable is an error listing every failure.
func ChooseTransport(probes []*TransportProbe) (*TransportProbe, error) {
	var udpP, tcpP *TransportProbe
	var reasons []string
	for _, p := range probes {
		if p.Err != nil {
			reasons = append(reasons, fmt.Sprintf("%s: %v", p.Transport, p.Err))
			continue
		}
		switch p.Transport {
		case "udp":
			udpP = p
		case "tcp":
			tcpP = p
		default:
			return p, nil
		}
	}
	if udpP != nil && (tcpP == nil || udpP.Mbps() >= 0.8*tcpP.Mbps()) {
		return udpP, nil
	}
	if tcpP != nil {
		return tcpP, nil
	}
	if udpP != nil {
		return udpP, nil
	}
	return nil, fmt.Errorf("all transports failed: %s", strings.Join(reasons, "; "))
}

// Entry builds a storable entry from a winning probe.
func (p *TransportProbe) Entry(name, server, token, fingerprint string) *Entry {
	return &Entry{
		Name:         name,
		Server:       server,
		Token:        token,
		Transport:    p.Transport,
		Fingerprint:  fingerprint,
		WGPrivateKey: p.Priv,
		WGPublicKey:  p.Pub,
		ServerPubKey: p.Resp.ServerPubKey,
		TunnelIP:     p.Resp.TunnelIP,
		ServerIP:     p.Resp.ServerIP,
		MTU:          p.Resp.MTU,
		Endpoint:     p.Resp.Endpoint,
		CreatedAt:    time.Now(),
	}
}
