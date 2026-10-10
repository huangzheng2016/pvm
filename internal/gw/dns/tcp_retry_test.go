package dns

import (
	"context"
	"net"
	"testing"
	"time"

	mdns "github.com/miekg/dns"
)

func TestUDPUpstreamRetriesTruncatedOverTCP(t *testing.T) {
	udpHandler := mdns.HandlerFunc(func(w mdns.ResponseWriter, req *mdns.Msg) {
		resp := mdns.Msg{}
		resp.SetReply(req)
		resp.Truncated = true
		_ = w.WriteMsg(&resp)
	})
	tcpHandler := mdns.HandlerFunc(func(w mdns.ResponseWriter, req *mdns.Msg) {
		resp := mdns.Msg{}
		resp.SetReply(req)
		q := req.Question[0]
		resp.Answer = append(resp.Answer, &mdns.A{
			Hdr: mdns.RR_Header{Name: q.Name, Rrtype: mdns.TypeA, Class: mdns.ClassINET, Ttl: 60},
			A:   net.ParseIP("192.0.2.55"),
		})
		_ = w.WriteMsg(&resp)
	})

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udpSrv := &mdns.Server{PacketConn: pc, Handler: udpHandler}
	go udpSrv.ActivateAndServe()
	defer udpSrv.Shutdown()
	addr := pc.LocalAddr().String()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	tcpSrv := &mdns.Server{Listener: ln, Handler: tcpHandler}
	go tcpSrv.ActivateAndServe()
	defer tcpSrv.Shutdown()

	up := UDPUpstream{Addr: addr}
	req := mdns.Msg{}
	req.SetQuestion("large.example.", mdns.TypeA)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := up.Exchange(ctx, &req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Truncated {
		t.Fatal("TCP retry response is still truncated")
	}
	if len(resp.Answer) != 1 || resp.Answer[0].(*mdns.A).A.String() != "192.0.2.55" {
		t.Fatalf("unexpected retry answer: %+v", resp.Answer)
	}
}
