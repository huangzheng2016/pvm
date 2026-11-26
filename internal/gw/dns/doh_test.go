package dns

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	mdns "github.com/miekg/dns"
)

func TestDoHUpstreamFailoverAndIDRestore(t *testing.T) {
	var primaryCalls atomic.Int32
	primary := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer primary.Close()

	backup := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/dns-message" {
			t.Error("missing DoH content type")
		}
		req := new(mdns.Msg)
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		if err := req.Unpack(buf); err != nil {
			t.Error(err)
			return
		}
		if req.Id != 0 {
			t.Errorf("wire ID = %d, want 0 (RFC 8484)", req.Id)
		}
		resp := new(mdns.Msg)
		resp.SetReply(req)
		packed, _ := resp.Pack()
		w.Header().Set("Content-Type", "application/dns-message")
		_, _ = w.Write(packed)
	}))
	defer backup.Close()

	up, err := NewDoHUpstream([]string{primary.URL, backup.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	up.client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}

	q := new(mdns.Msg)
	q.SetQuestion("example.com.", mdns.TypeA)
	q.Id = 4242
	resp, err := up.Exchange(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Id != 4242 {
		t.Fatalf("response ID = %d, want 4242 restored", resp.Id)
	}
	// Second query should prefer the healthy backup without re-hitting the 429 primary.
	if _, err := up.Exchange(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if primaryCalls.Load() != 1 {
		t.Fatalf("primary was retried before the 5m window: %d calls", primaryCalls.Load())
	}
}
