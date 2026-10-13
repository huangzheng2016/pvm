package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/tunnel"
)

func TestStandardWGConfig(t *testing.T) {
	priv, err := tunnel.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub, err := tunnel.PublicKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	conf, err := standardWGConfig(priv, "10.99.0.2", pub, "exit.example.com:18443", 1420)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"PrivateKey = " + priv,
		"Address = 10.99.0.2/32",
		"PublicKey = " + pub,
		"Endpoint = exit.example.com:18443",
		"AllowedIPs = 0.0.0.0/0",
		"PersistentKeepalive = 25",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("config missing %q:\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "::/0") {
		t.Fatal("IPv6 must not be routed into the IPv4-only tunnel")
	}
	for _, tc := range []struct {
		ip, endpoint string
		mtu          int
	}{
		{"10.99.0.2\nDNS = 8.8.8.8", "exit.example.com:18443", 1420},
		{"10.99.0.2", "exit.example.com:18443\nAllowedIPs = 0.0.0.0/0", 1420},
		{"10.99.0.2", "exit.example.com:0", 1420},
		{"10.99.0.2", "exit.example.com:18443", 0},
	} {
		if _, err := standardWGConfig(priv, tc.ip, pub, tc.endpoint, tc.mtu); err == nil {
			t.Errorf("accepted invalid config input: %+v", tc)
		}
	}
}

func TestExitClientConfig(t *testing.T) {
	priv, err := tunnel.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	serverPub, err := tunnel.PublicKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	pubCh := make(chan string, 1)
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/enroll" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req struct {
			PubKey    string `json:"wg_pubkey"`
			Transport string `json:"transport"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.Transport != "udp" {
			http.Error(w, "wrong transport", http.StatusBadRequest)
			return
		}
		pubCh <- req.PubKey
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"server_pubkey": serverPub,
			"tunnel_ip":     "10.99.0.2",
			"server_ip":     "10.99.0.1",
			"mtu":           1420,
			"endpoint":      "203.0.113.1:18443",
		})
	}))
	defer ts.Close()
	fp, err := exit.Fingerprint(ts.Certificate().Raw)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	t.Setenv("PVM_HOME", home)
	if err := exit.SaveEntry(filepath.Join(home, "exits"), &exit.Entry{
		Name:         "us",
		Server:       strings.TrimPrefix(ts.URL, "https://"),
		Token:        "test-token",
		Transport:    "udp",
		Fingerprint:  fp,
		WGPrivateKey: priv,
	}); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "phone.conf")
	var qr bytes.Buffer
	if code := exitClientConfigTo([]string{"us", "--output", output}, &qr); code != 0 {
		t.Fatalf("client-config exit code = %d", code)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o, want 600", fi.Mode().Perm())
	}
	conf := string(data)
	if !strings.Contains(conf, "Address = 10.99.0.2/32") || !strings.Contains(conf, "Endpoint = 203.0.113.1:18443") {
		t.Fatalf("unexpected config:\n%s", conf)
	}
	var clientPriv string
	for _, line := range strings.Split(conf, "\n") {
		if strings.HasPrefix(line, "PrivateKey = ") {
			clientPriv = strings.TrimPrefix(line, "PrivateKey = ")
		}
	}
	clientPub, err := tunnel.PublicKey(clientPriv)
	if err != nil {
		t.Fatal(err)
	}
	if enrolled := <-pubCh; enrolled != clientPub {
		t.Fatalf("enrolled pubkey does not match exported private key")
	}
	if clientPriv == priv {
		t.Fatal("exported config reused the pvm gateway private key")
	}
	if !strings.Contains(qr.String(), "▄") || !strings.Contains(qr.String(), "wrote "+output) {
		t.Fatalf("missing terminal QR or output path: %q", qr.String())
	}
}

func TestExitClientConfigRejectsTCP(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PVM_HOME", home)
	if err := exit.SaveEntry(filepath.Join(home, "exits"), &exit.Entry{Name: "us-tcp", Transport: "tcp"}); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "client.conf")
	if code := exitClientConfigTo([]string{"us-tcp", "--output", output}, &bytes.Buffer{}); code == 0 {
		t.Fatal("TCP exit must not generate a standard WireGuard config")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("unexpected config file: %v", err)
	}
}
