package gateway

import (
	"context"
	"net"
	"testing"
	"time"

	mdns "github.com/miekg/dns"
)

func TestTCPUpstream(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &mdns.Server{Listener: ln, Handler: mdns.HandlerFunc(func(w mdns.ResponseWriter, req *mdns.Msg) {
		resp := new(mdns.Msg)
		resp.SetReply(req)
		resp.Answer = []mdns.RR{&mdns.A{Hdr: mdns.RR_Header{Name: req.Question[0].Name, Rrtype: mdns.TypeA, Class: mdns.ClassINET}, A: net.ParseIP("192.0.2.1")}}
		_ = w.WriteMsg(resp)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	up, err := dnsUpstream("tcp://" + ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := new(mdns.Msg)
	req.SetQuestion("example.com.", mdns.TypeA)
	resp, err := up.Exchange(ctx, req)
	if err != nil || len(resp.Answer) != 1 {
		t.Fatalf("TCP DNS exchange: %v, response=%v", err, resp)
	}
	resolver := upstreamResolver("tcp://" + ln.Addr().String())
	ips, err := resolver.LookupIP(ctx, "ip4", "example.com")
	if err != nil || len(ips) != 1 || !ips[0].Equal(net.ParseIP("192.0.2.1")) {
		t.Fatalf("resolver did not use TCP: %v, ips=%v", err, ips)
	}
	if _, err := dnsUpstream("tls://1.1.1.1:853"); err == nil {
		t.Fatal("unsupported DNS scheme was silently accepted")
	}
}
