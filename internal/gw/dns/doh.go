package dns

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mdns "github.com/miekg/dns"
)

// DoHUpstream resolves DNS over HTTPS (RFC 8484) across primary and backup
// endpoints. It remembers the last healthy endpoint, bounds untried backups to
// 2.5s so glibc's 5s resolver timeout is not hit, and lets a single query
// re-probe the primary every 5 minutes. It never falls back to plain DNS.
type DoHUpstream struct {
	Endpoints []string
	Dial      func(ctx context.Context, network, address string) (net.Conn, error)

	client      *http.Client
	mu          sync.Mutex
	preferred   int
	tried       map[int]bool
	lastPrimary time.Time
	probingCAS  atomic.Bool
}

func NewDoHUpstream(endpoints []string, dial func(context.Context, string, string) (net.Conn, error)) (*DoHUpstream, error) {
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("at least one DoH endpoint is required")
	}
	for _, ep := range endpoints {
		u, err := url.Parse(ep)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return nil, fmt.Errorf("invalid DoH endpoint %q (HTTPS without credentials required)", ep)
		}
	}
	tr := &http.Transport{DialContext: dial, ForceAttemptHTTP2: true, MaxIdleConnsPerHost: 4, IdleConnTimeout: 60 * time.Second}
	return &DoHUpstream{
		Endpoints: append([]string(nil), endpoints...),
		Dial:      dial,
		client: &http.Client{
			Transport: tr,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return fmt.Errorf("DoH redirect refused")
			},
		},
		tried: map[int]bool{},
	}, nil
}

func (u *DoHUpstream) order() (order []int, timeout time.Duration, probePrimary bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	timeout = 4 * time.Second
	order = append(order, u.preferred)
	probePrimary = u.preferred != 0 && time.Since(u.lastPrimary) >= 5*time.Minute && u.probingCAS.CompareAndSwap(false, true)
	if probePrimary {
		u.lastPrimary = time.Now()
		order = append([]int{0}, order...)
	} else if u.preferred == 0 && u.lastPrimary.IsZero() {
		u.lastPrimary = time.Now()
	}
	for i := range u.Endpoints {
		if i == u.preferred || probePrimary && i == 0 {
			continue
		}
		order = append(order, i)
	}
	for _, idx := range order {
		if !u.tried[idx] {
			timeout = 2500 * time.Millisecond
			break
		}
	}
	return order, timeout, probePrimary
}

func (u *DoHUpstream) Exchange(ctx context.Context, req *mdns.Msg) (*mdns.Msg, error) {
	wire := req.Copy()
	wire.Id = 0
	packed, err := wire.Pack()
	if err != nil {
		return nil, err
	}
	order, attemptTimeout, probing := u.order()
	if probing {
		defer u.probingCAS.Store(false)
	}
	var lastErr error
	for _, idx := range order {
		resp, err := u.attempt(ctx, idx, attemptTimeout, packed)
		if err == nil && resp != nil {
			if rcodeBad(resp.Rcode) {
				lastErr = fmt.Errorf("DoH %s: rcode %s", hostOnly(u.Endpoints[idx]), mdns.RcodeToString[resp.Rcode])
				continue
			}
			resp.Id = req.Id
			u.mu.Lock()
			u.preferred = idx
			u.tried[idx] = true
			if idx == 0 {
				u.lastPrimary = time.Now()
			}
			u.mu.Unlock()
			return resp, nil
		}
		if err != nil {
			lastErr = err
		}
		u.mu.Lock()
		u.tried[idx] = true
		u.mu.Unlock()
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no DoH endpoint available")
	}
	return nil, lastErr
}

func (u *DoHUpstream) attempt(parent context.Context, idx int, d time.Duration, packed []byte) (*mdns.Msg, error) {
	ctx, cancel := context.WithTimeout(parent, d)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.Endpoints[idx], bytes.NewReader(packed))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return nil, fmt.Errorf("DoH %s: %s", hostOnly(u.Endpoints[idx]), resp.Status)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DoH %s: %s", hostOnly(u.Endpoints[idx]), resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if err != nil {
		return nil, err
	}
	msg := new(mdns.Msg)
	if err := msg.Unpack(body); err != nil {
		return nil, err
	}
	return msg, nil
}

func rcodeBad(rcode int) bool {
	return rcode == mdns.RcodeServerFailure || rcode == mdns.RcodeRefused || rcode == mdns.RcodeFormatError
}

// ServeSingleConn adapts a miekg/dns DNS stream (TCP length-prefixed or UDP
// packet over net.Pipe) to an Upstream so net.Resolver can use DoH endpoints.
func ServeSingleConn(ctx context.Context, conn net.Conn, up Upstream) error {
	tcp := &mdns.Client{Net: "tcp", Timeout: 5 * time.Second}
	_ = tcp
	for {
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		var hdr [2]byte
		if _, err := io.ReadFull(conn, hdr[:]); err != nil {
			return err
		}
		n := int(hdr[0])<<8 | int(hdr[1])
		if n <= 0 || n > 65535 {
			return fmt.Errorf("invalid DNS frame length %d", n)
		}
		buf := make([]byte, n)
		if _, err := io.ReadFull(conn, buf); err != nil {
			return err
		}
		req := new(mdns.Msg)
		if err := req.Unpack(buf); err != nil {
			return err
		}
		resp, err := up.Exchange(ctx, req)
		if err != nil || resp == nil {
			resp = new(mdns.Msg)
			resp.SetRcode(req, mdns.RcodeServerFailure)
		}
		resp.Id = req.Id
		out, err := resp.Pack()
		if err != nil {
			return err
		}
		if len(out) > 65535 {
			return fmt.Errorf("oversized DNS response")
		}
		frame := append([]byte{byte(len(out) >> 8), byte(len(out))}, out...)
		if _, err := conn.Write(frame); err != nil {
			return err
		}
	}
}

func hostOnly(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return strings.SplitN(raw, "/", 4)[0]
}
