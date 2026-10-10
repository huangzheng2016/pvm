package exit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/tunnel"
)

func TestParseIpcLastHandshake(t *testing.T) {
	dump := `private_key=aaaa
public_key=aaaa1111
listen_port=18443
public_key=bbbb2222
endpoint=1.2.3.4:5000
allowed_ip=10.99.0.2/32
last_handshake_time_sec=1760000000
last_handshake_time_nsec=123
public_key=cccc3333
allowed_ip=10.99.0.3/32
last_handshake_time_sec=0
last_handshake_time_nsec=0
`
	got := parseIpcLastHandshake(dump, "bbbb2222")
	if got.Unix() != 1760000000 {
		t.Fatalf("handshake = %v", got)
	}
	if !parseIpcLastHandshake(dump, "cccc3333").IsZero() {
		t.Fatal("sec=0 must mean never")
	}
	if !parseIpcLastHandshake(dump, "dddd4444").IsZero() {
		t.Fatal("absent peer must mean never")
	}
	if !parseIpcLastHandshake("", "bbbb2222").IsZero() {
		t.Fatal("empty dump")
	}
}

// Enroll then delete: the peer must be gone from BOTH the store and the
// wireguard device (device removal used to be fire-and-forget).
func TestDeletePeerRemovesFromDevice(t *testing.T) {
	srv, _ := inProcessServer(t, "udp")
	serverAddr := srv.ControlAddr()
	fp, err := ProbeFingerprint(serverAddr)
	if err != nil {
		t.Fatal(err)
	}
	priv, err := tunnel.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := tunnel.PublicKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(serverAddr, "test-token", fp)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := client.Enroll(ctx, pub, "gw-test", "udp")
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if !deviceHasPeer(srv, "udp", resp.TunnelIP) {
		t.Fatal("peer not on device after enroll")
	}
	if err := client.DeletePeer(ctx, resp.TunnelIP); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if deviceHasPeer(srv, "udp", resp.TunnelIP) {
		t.Fatal("peer still on device after delete")
	}
	if _, ok := srv.store.Get(resp.TunnelIP); ok {
		t.Fatal("peer still in store after delete")
	}
}

func deviceHasPeer(s *Server, transport, ip string) bool {
	t, ok := s.tr[transport]
	if !ok {
		return false
	}
	out, err := t.dev.IpcGet()
	if err != nil {
		return false
	}
	return strings.Contains(out, "allowed_ip="+ip+"/32")
}

// A client with an empty pinned fingerprint must fail closed, not skip
// certificate verification.
func TestClientEmptyFingerprintFails(t *testing.T) {
	srv, _ := inProcessServer(t, "udp")
	c := NewClient(srv.ControlAddr(), "test-token", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := c.Health(ctx)
	if err == nil || !strings.Contains(err.Error(), "no pinned server certificate fingerprint") {
		t.Fatalf("want pinned-fingerprint error, got %v", err)
	}
}

var _ = time.Second
