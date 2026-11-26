package netcheck

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProbeExitIPFailsOverFastAndValidatesPublicIPv4(t *testing.T) {
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "blocked", http.StatusForbidden)
	}))
	defer fail.Close()
	private := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("192.168.1.1\n"))
	}))
	defer private.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("203.0.113.42\n"))
	}))
	defer good.Close()

	start := time.Now()
	ip, svc, err := ProbeExitIP(context.Background(), nil, []EchoService{
		{Name: "fail", URL: fail.URL},
		{Name: "private", URL: private.URL},
		{Name: "good", URL: good.URL},
	})
	if err != nil || ip != "203.0.113.42" || svc != "good" {
		t.Fatalf("ProbeExitIP = %q, %q, %v", ip, svc, err)
	}
	if time.Since(start) >= time.Second {
		t.Fatalf("fast failover waited for stagger: %v", time.Since(start))
	}
}

func TestExitPolicyEvaluation(t *testing.T) {
	static := &ExitPolicy{Mode: "static", AnchorIP: "203.0.113.10"}
	geo := &GeoInfo{IP: "203.0.113.10", CountryCode: "US", Region: "CA", City: "San Jose"}
	if err := static.Evaluate("203.0.113.10", geo); err != nil {
		t.Fatal(err)
	}
	if err := static.Evaluate("203.0.113.11", geo); err == nil {
		t.Fatal("static IP drift was allowed")
	}
	if err := static.Evaluate("203.0.113.10", &GeoInfo{CountryCode: "CN"}); err == nil {
		t.Fatal("mainland CN exit was allowed")
	}
	if err := static.Evaluate("203.0.113.10", nil); err == nil {
		t.Fatal("unknown country was allowed")
	}
	city := &ExitPolicy{Mode: "dynamic", Scope: "city", Country: "US", Region: "CA", City: "San Jose"}
	if err := city.Evaluate("203.0.113.99", geo); err != nil {
		t.Fatal(err)
	}
	if err := city.Evaluate("203.0.113.99", &GeoInfo{CountryCode: "US", Region: "CA", City: "LA"}); err == nil {
		t.Fatal("city drift was allowed")
	}
}

func TestReportAuthorizesRequiresEvidence(t *testing.T) {
	r := NewReport("dev1", "boot-1", "hash-1")
	if r.Authorizes("boot-1", "hash-1") {
		t.Fatal("empty report authorized")
	}
	r.Checks = []CheckItem{{ID: "dns", State: StatePassed, Critical: true, Evidence: "ok", CheckedAt: time.Now()}}
	if !r.Authorizes("boot-1", "hash-1") || r.Authorizes("boot-2", "hash-1") {
		t.Fatal("boot binding failed")
	}
	r.Checks = append(r.Checks, CheckItem{ID: "v6", State: StateUnknown, Critical: true, Evidence: "", CheckedAt: time.Now()})
	if r.Authorizes("boot-1", "hash-1") {
		t.Fatal("unknown critical check authorized")
	}
}
