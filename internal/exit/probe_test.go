package exit

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func probeResult(transport string, mbps float64, err error) *TransportProbe {
	p := &TransportProbe{Transport: transport, Err: err}
	if err == nil {
		p.Bytes = int64(mbps * 1e6)
		p.Duration = time.Second
	}
	return p
}

func TestChooseTransport(t *testing.T) {
	cases := []struct {
		name    string
		probes  []*TransportProbe
		want    string
		wantErr bool
	}{
		{"udp preferred when comparable", []*TransportProbe{probeResult("udp", 90, nil), probeResult("tcp", 100, nil)}, "udp", false},
		{"udp at exactly 80pct", []*TransportProbe{probeResult("udp", 80, nil), probeResult("tcp", 100, nil)}, "udp", false},
		{"tcp wins when udp below 80pct", []*TransportProbe{probeResult("udp", 79, nil), probeResult("tcp", 100, nil)}, "tcp", false},
		{"udp only", []*TransportProbe{probeResult("udp", 50, nil), probeResult("tcp", 0, errors.New("dial timeout"))}, "udp", false},
		{"tcp only", []*TransportProbe{probeResult("udp", 0, errors.New("blocked")), probeResult("tcp", 10, nil)}, "tcp", false},
		{"all failed", []*TransportProbe{probeResult("udp", 0, errors.New("e1")), probeResult("tcp", 0, errors.New("e2"))}, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, err := ChooseTransport(c.probes)
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got %v", w.Transport)
				}
				if !strings.Contains(err.Error(), "e1") || !strings.Contains(err.Error(), "e2") {
					t.Fatalf("error should list all reasons: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if w.Transport != c.want {
				t.Fatalf("got %s, want %s", w.Transport, c.want)
			}
		})
	}
}

func TestSpeedEndpoint(t *testing.T) {
	srv := httptest.NewServer(speedHandler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/speed?bytes=1024")
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if err != nil || n != 1024 {
		t.Fatalf("got %d bytes (err %v), want 1024", n, err)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("missing no-store")
	}

	// over the cap -> capped at 64MB via Content-Length
	resp, err = http.Get(srv.URL + fmt.Sprintf("/speed?bytes=%d", 128<<20))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.ContentLength; got != maxSpeedBytes {
		t.Fatalf("ContentLength = %d, want %d", got, maxSpeedBytes)
	}

	resp, err = http.Get(srv.URL + "/ping")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"ok":true`) || !strings.Contains(string(body), `"ts":`) {
		t.Fatalf("bad ping body: %s", body)
	}
}

// serveSpeedConn answers raw connections (userspace intercept path).
func TestServeSpeedConn(t *testing.T) {
	server, client := net.Pipe()
	go serveSpeedConn(server)
	_, err := fmt.Fprintf(client, "GET /speed?bytes=100 HTTP/1.1\r\nHost: tunnel\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(client)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status, "200") {
		t.Fatalf("status: %s", status)
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if line == "\r\n" {
			break
		}
	}
	n, err := io.CopyN(io.Discard, br, 100)
	if err != nil || n != 100 {
		t.Fatalf("got %d bytes (err %v), want 100", n, err)
	}
	client.Close()
}

// ProbeTransport against an in-process userspace server: full enroll,
// handshake, ping, download, and the intercept-carried speed endpoint.
func TestProbeInProcessUserspace(t *testing.T) {
	srv := startUserspaceServer(t, "both")
	fp, err := ProbeFingerprint(srv.ControlAddr())
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	probes := ProbeAll(ctx, srv.ControlAddr(), "test-token", fp, []string{"udp", "tcp"}, 1<<20)
	for _, p := range probes {
		if p.Err != nil {
			t.Fatalf("%s probe: %v", p.Transport, p.Err)
		}
		if p.Bytes != 1<<20 {
			t.Fatalf("%s: got %d bytes, want 1MB", p.Transport, p.Bytes)
		}
		if p.RTT <= 0 {
			t.Fatalf("%s: rtt not measured", p.Transport)
		}
		t.Logf("%s: %.2f MB/s rtt %s", p.Transport, p.Mbps(), p.RTT)
	}
	winner, err := ChooseTransport(probes)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range probes {
		if p != winner {
			RevokeProbe(ctx, srv.ControlAddr(), "test-token", fp, p)
		}
	}
	if got := len(srv.store.List()); got != 1 {
		t.Fatalf("peers after revoke = %d, want 1 (winner only)", got)
	}
}
