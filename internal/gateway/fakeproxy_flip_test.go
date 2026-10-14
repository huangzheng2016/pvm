package gateway

import (
	"net"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/gw/dns"
	"github.com/huangzheng2016/pvm/internal/gw/policy"
	"github.com/huangzheng2016/pvm/internal/gw/rules"
)

func newFlipTestPolicy(t *testing.T) *policy.Engine {
	t.Helper()
	re, err := rules.NewEngine("", nil)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := dns.NewPool("198.18.0.0/16", 0)
	if err != nil {
		t.Fatal(err)
	}
	return policy.New(re, pool)
}

// A direct -> proxy policy flip must reset live flows so they reconnect
// via the new route instead of leaking on the old path.
func TestSetPolicyResetsFlippedConnections(t *testing.T) {
	eng := newFlipTestPolicy(t)
	p := newFakeProxy(eng, "10.0.2.3", nil)

	src, up := net.Pipe()
	defer src.Close()
	defer up.Close()
	tc := &trackedConn{vm: "dev1", domain: "example.com", route: policy.Direct, src: src, up: up}
	p.track(tc)

	// unaffected flow from another vm stays up
	src2, up2 := net.Pipe()
	defer src2.Close()
	defer up2.Close()
	tc2 := &trackedConn{vm: "dev2", domain: "example.com", route: policy.Direct, src: src2, up: up2}
	p.track(tc2)

	// dev1's route flips to proxy
	eng.SetVMExit("dev1", "my-exit")
	p.SetPolicy(eng)

	assertClosed(t, src, "flipped connection must be reset")
	assertOpen(t, src2, "unaffected connection must stay open")
}

// A proxied flow whose exit tunnel goes down is reset immediately
// (fail-closed reconnect), not left hanging until the idle timeout.
func TestSetExitHooksResetsDeadTunnelConnections(t *testing.T) {
	eng := newFlipTestPolicy(t)
	p := newFakeProxy(eng, "10.0.2.3", nil)

	src, up := net.Pipe()
	defer src.Close()
	defer up.Close()
	p.track(&trackedConn{vm: "dev1", domain: "example.com", route: policy.Proxy, exit: "my-exit", src: src, up: up})

	// direct flows never reference tunnels and must survive
	src2, up2 := net.Pipe()
	defer src2.Close()
	defer up2.Close()
	p.track(&trackedConn{vm: "dev2", domain: "example.com", route: policy.Direct, src: src2, up: up2})

	// tunnel "my-exit" is gone (markFor = -1 for every exit)
	p.SetExitHooks(func(string) int { return -1 }, func(string) (string, bool) { return "", false })

	assertClosed(t, src, "flow via dead tunnel must be reset")
	assertOpen(t, src2, "direct flow must stay open")
}

// Flows whose decision did NOT change survive a policy reload with
// the same content (steady-state applies must be no-ops).
func TestSetPolicyKeepsUnchangedConnections(t *testing.T) {
	eng := newFlipTestPolicy(t)
	eng.SetVMExit("dev1", "my-exit")
	p := newFakeProxy(eng, "10.0.2.3", nil)

	src, up := net.Pipe()
	defer src.Close()
	defer up.Close()
	p.track(&trackedConn{vm: "dev1", domain: "example.com", route: policy.Proxy, exit: "my-exit", src: src, up: up})

	p.SetPolicy(eng) // same policy content
	assertOpen(t, src, "unchanged flow must survive reload")
}

func assertClosed(t *testing.T, c net.Conn, msg string) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	_, err := c.Read(make([]byte, 1))
	if err == nil {
		t.Fatal(msg)
	}
}

func assertOpen(t *testing.T, c net.Conn, msg string) {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, err := c.Read(make([]byte, 1))
	if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatalf("%s (got err=%v, want timeout)", msg, err)
	}
}
