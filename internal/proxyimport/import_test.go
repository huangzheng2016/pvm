package proxyimport

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestParseClashSanitizesAndBuildsChain(t *testing.T) {
	yamlInput := `proxies:
  - name: relay
    type: ss
    server: 203.0.113.10
    port: 8388
    cipher: aes-256-gcm
    password: YOUR_TOKEN
    interface-name: en0
  - name: exit
    type: vless
    server: 203.0.113.20
    port: 443
    uuid: 11111111-2222-4333-8444-555555555555
    tls: true
  - name: local-bad
    type: socks5
    server: 127.0.0.1
    port: 7890
`
	src, err := Parse("sub", []byte(yamlInput))
	if err != nil {
		t.Fatal(err)
	}
	if len(src.Proxies) != 2 || len(src.Skipped) != 1 {
		t.Fatalf("proxies=%d skipped=%v", len(src.Proxies), src.Skipped)
	}
	if _, ok := src.Proxies[0]["interface-name"]; ok {
		t.Fatal("interface-name was not stripped")
	}
	cfg, err := BuildMihomoConfig(src, "", []string{"relay", "exit"}, 17890)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(cfg, &doc); err != nil {
		t.Fatal(err)
	}
	text := string(cfg)
	if !strings.Contains(text, "dialer-proxy: pvm-hop-0") || !strings.Contains(text, "allow-lan: false") || !strings.Contains(text, "socks-port: 17890") {
		t.Fatalf("unexpected Mihomo config:\n%s", text)
	}
}

func TestParseShareLinks(t *testing.T) {
	src, err := Parse("links", []byte("trojan://pass@203.0.113.30:443?sni=example.com#node1\nvless://11111111-2222-4333-8444-555555555555@203.0.113.40:443?security=reality&pbk=abc&sid=01#node2\n"))
	if err != nil || len(src.Proxies) != 2 {
		t.Fatalf("Parse share links = %+v, %v", src, err)
	}
}
