package warp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/tunnel"
)

func TestRegisterEntryAndDelete(t *testing.T) {
	serverPrivate, err := tunnel.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	serverPublic, err := tunnel.PublicKey(serverPrivate)
	if err != nil {
		t.Fatal(err)
	}
	clientPrivate, err := tunnel.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	clientPublic, err := tunnel.PublicKey(clientPrivate)
	if err != nil {
		t.Fatal(err)
	}
	deleted := false
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v0a5641/reg":
			if r.Header.Get("CF-Client-Version") != "a-6.38.9-5641" {
				t.Error("missing Cloudflare client version")
			}
			var req map[string]any
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			if req["key"] != clientPublic || req["tos"] == "" || req["tunnel_type"] != "wireguard" {
				t.Errorf("unexpected registration request: %v", req)
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"id":    "device-1",
				"token": "device-token",
				"account": map[string]any{
					"license": "license-1",
				},
				"config": map[string]any{
					"interface": map[string]any{
						"addresses": map[string]any{"v4": "172.16.0.2/32", "v6": "2606:4700:110::2/128"},
					},
					"peers": []any{map[string]any{
						"public_key": serverPublic,
						"endpoint":   map[string]any{"host": "engage.cloudflareclient.com:2408"},
					}},
				},
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/v0a5641/reg/device-1":
			deleted = true
			if r.Header.Get("Authorization") != "Bearer device-token" {
				t.Error("missing device token")
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	client := NewClient(ts.URL)
	registration, err := client.Register(context.Background(), clientPublic)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := NewEntry("warp-test", clientPrivate, registration)
	if err != nil {
		t.Fatal(err)
	}
	if !entry.IsWARP() || entry.Provider != exit.ProviderWARP {
		t.Fatalf("provider = %q", entry.Provider)
	}
	if entry.TunnelIP != "172.16.0.2" || entry.Endpoint != "engage.cloudflareclient.com:2408" || entry.MTU != 1280 {
		t.Fatalf("unexpected data-plane config: %+v", entry)
	}
	if entry.WARP == nil || entry.WARP.DeviceID != "device-1" || entry.WARP.Token != "device-token" || entry.WARP.IPv6 == "" {
		t.Fatalf("missing account state: %+v", entry.WARP)
	}
	if entry.Token != "" || entry.ServerIP != "" {
		t.Fatal("VPS-only fields must stay empty")
	}
	if err := client.Delete(context.Background(), registration.ID, registration.Token); err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Fatal("device delete was not called")
	}
}

func TestRegisterFetchesMissingDeviceConfig(t *testing.T) {
	private, err := tunnel.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	public, err := tunnel.PublicKey(private)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			json.NewEncoder(w).Encode(map[string]any{"id": "device-1", "token": "device-token", "account": map[string]any{}})
		case http.MethodGet:
			if r.URL.Path != "/v0a5641/reg/device-1" || r.Header.Get("Authorization") != "Bearer device-token" {
				t.Errorf("unexpected device request: %s %s", r.Method, r.URL.Path)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"config": map[string]any{
					"interface": map[string]any{"addresses": map[string]any{"v4": "172.16.0.2"}},
					"peers": []any{map[string]any{
						"public_key": public,
						"endpoint":   map[string]any{"host": "203.0.113.10:2408"},
					}},
				},
			})
		}
	}))
	defer ts.Close()
	registration, err := NewClient(ts.URL).Register(context.Background(), public)
	if err != nil {
		t.Fatal(err)
	}
	if registration.ID != "device-1" || registration.Token != "device-token" || len(registration.Config.Peers) != 1 {
		t.Fatalf("unexpected merged registration: %+v", registration)
	}
}

func TestRegisterErrorIncludesResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key", http.StatusBadRequest)
	}))
	defer ts.Close()
	_, err := NewClient(ts.URL).Register(context.Background(), "bad")
	if err == nil || !strings.Contains(err.Error(), "400") || !strings.Contains(err.Error(), "bad key") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestEndpointString(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint Endpoint
		want     string
	}{
		{"host with port", Endpoint{Host: "engage.cloudflareclient.com:2408"}, "engage.cloudflareclient.com:2408"},
		{"ipv4 and ports", Endpoint{V4: "203.0.113.1", Ports: []int{500}}, "203.0.113.1:500"},
		{"ipv6 default port", Endpoint{V6: "2001:db8::1"}, "[2001:db8::1]:2408"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.endpoint.String()
			if err != nil || got != tc.want {
				t.Fatalf("String() = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
	for _, endpoint := range []Endpoint{{}, {Host: "example.com:0"}, {V4: "203.0.113.1", Ports: []int{70000}}} {
		if _, err := endpoint.String(); err == nil {
			t.Fatalf("accepted invalid endpoint: %+v", endpoint)
		}
	}
}

func TestNewEntryRejectsIncompleteRegistration(t *testing.T) {
	private, err := tunnel.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewEntry("warp-test", private, nil); err == nil {
		t.Fatal("nil registration must fail")
	}
	if _, err := NewEntry("warp-test", private, &Registration{ID: "device", Token: "token"}); err == nil {
		t.Fatal("registration without a peer must fail")
	}
}
