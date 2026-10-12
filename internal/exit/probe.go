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

// probeHandshakeTimeout marks a transport unusable when its handshake does
// not complete in time (relaxed in -race test runs).
var probeHandshakeTimeout = 10 * time.Second

// SpeedSample is one ping+download round against the speed endpoint.
type SpeedSample struct {
	RTT  time.Duration
	Mbps float64
	Err  error
}

// SampleSpeed runs n ping+download rounds against the tunnel speed
// endpoint. Individual failures are recorded per sample.
func (t *Tunnel) SampleSpeed(ctx context.Context, serverIP netip.Addr, bytesN int64, n int) []SpeedSample {
	if n < 1 {
		n = 1
	}
	out := make([]SpeedSample, 0, n)
	for i := 0; i < n; i++ {
		var s SpeedSample
		t0 := time.Now()
		if _, err := t.Ping(ctx, serverIP); err != nil {
			s.Err = fmt.Errorf("ping: %w", err)
			out = append(out, s)
			continue
		}
		s.RTT = time.Since(t0)
		nbytes, dur, err := t.Download(ctx, serverIP, fmt.Sprintf("/speed?bytes=%d", bytesN))
		if err != nil {
			s.Err = fmt.Errorf("download: %w", err)
			out = append(out, s)
			continue
		}
		s.Mbps = float64(nbytes) / dur.Seconds() / 1e6
		out = append(out, s)
	}
	return out
}

// AggregateSamples averages the successful samples; an error is returned
// only when every sample failed.
func AggregateSamples(samples []SpeedSample) (avgRTT time.Duration, avgMbps float64, err error) {
	var cnt int
	var rttSum time.Duration
	var mbpsSum float64
	var lastErr error
	for _, s := range samples {
		if s.Err != nil {
			lastErr = s.Err
			continue
		}
		cnt++
		rttSum += s.RTT
		mbpsSum += s.Mbps
	}
	if cnt == 0 {
		if lastErr == nil {
			lastErr = fmt.Errorf("no samples")
		}
		return 0, 0, lastErr
	}
	return rttSum / time.Duration(cnt), mbpsSum / float64(cnt), nil
}

// TransportProbe is the outcome of probing one transport end-to-end:
// enroll, handshake, and sampled ping RTT + tunneled downloads.
type TransportProbe struct {
	Transport string
	Priv      string
	Pub       string
	Resp      *enrollResponse
	RTT       time.Duration // average across successful samples
	AvgMbps   float64       // average across successful samples
	Samples   []SpeedSample
	Err       error
}

// Mbps returns the measured average download throughput in MB/s.
func (p *TransportProbe) Mbps() float64 {
	if p.Err != nil {
		return 0
	}
	return p.AvgMbps
}

// ProbeTransport enrolls with one transport and measures it. The probe
// keeps its enrolled peer — the caller either builds an Entry from it
// (winner) or revokes it (loser) via RevokeProbe.
func ProbeTransport(ctx context.Context, server, token, fingerprint, transport string, bytesN int64, samples int) *TransportProbe {
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
	hctx, hcancel := context.WithTimeout(ctx, probeHandshakeTimeout)
	defer hcancel()
	if err := tun.WaitHandshake(hctx, serverIP); err != nil {
		return fail(fmt.Errorf("handshake: %w", err))
	}

	sip := netip.MustParseAddr(serverIP)
	p.Samples = tun.SampleSpeed(ctx, sip, bytesN, samples)
	rtt, mbps, err := AggregateSamples(p.Samples)
	if err != nil {
		return fail(err)
	}
	p.RTT, p.AvgMbps = rtt, mbps
	return p
}

// ProbeAll probes every given transport in parallel.
func ProbeAll(ctx context.Context, server, token, fingerprint string, transports []string, bytesN int64, samples int) []*TransportProbe {
	out := make([]*TransportProbe, len(transports))
	var wg sync.WaitGroup
	for i, tr := range transports {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = ProbeTransport(ctx, server, token, fingerprint, tr, bytesN, samples)
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
