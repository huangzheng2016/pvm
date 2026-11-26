// Package proxyimport parses and sanitizes Clash/Mihomo/Surge configurations
// and share links for execution inside the gateway VM's Mihomo process.
package proxyimport

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Skip struct {
	Name   string `json:"name"`
	Reason string `json:"reason"` // never contains credentials or URLs
}

type Source struct {
	Name    string           `json:"name"`
	Proxies []map[string]any `json:"proxies"`
	Skipped []Skip           `json:"skipped,omitempty"`
}

var allowedTypes = map[string]bool{
	"ss": true, "ssr": true, "vmess": true, "vless": true, "trojan": true,
	"hysteria2": true, "hysteria": true, "tuic": true, "socks5": true,
	"http": true, "ssh": true, "anytls": true, "snell": true,
}

var forbiddenKeys = []string{"interface-name", "routing-mark", "tun", "listeners"}

func isLocalServer(host string) bool {
	h := strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]"))
	if h == "" || h == "localhost" || strings.HasSuffix(h, ".local") || strings.HasSuffix(h, ".internal") {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast()
	}
	// Reject integer/hex IPv4 spellings like 2130706433 or 0x7f000001.
	if !strings.Contains(h, ".") && !strings.Contains(h, ":") {
		if _, err := strconv.ParseUint(h, 0, 64); err == nil {
			return true
		}
	}
	return false
}

func Sanitize(raw []map[string]any) ([]map[string]any, []Skip) {
	var kept []map[string]any
	var skipped []Skip
	seen := map[string]bool{}
	for _, p := range raw {
		name, _ := p["name"].(string)
		name = strings.TrimSpace(name)
		typ, _ := p["type"].(string)
		server, _ := p["server"].(string)
		if name == "" || seen[name] {
			skipped = append(skipped, Skip{Name: name, Reason: "empty or duplicate node name"})
			continue
		}
		if strings.ContainsAny(name, "\r\n\x00") {
			skipped = append(skipped, Skip{Name: "invalid", Reason: "control characters in node name"})
			continue
		}
		if !allowedTypes[strings.ToLower(typ)] {
			skipped = append(skipped, Skip{Name: name, Reason: fmt.Sprintf("unsupported proxy type %q", typ)})
			continue
		}
		if isLocalServer(server) {
			skipped = append(skipped, Skip{Name: name, Reason: "loopback or local proxy server cannot be migrated automatically"})
			continue
		}
		portOK := false
		switch v := p["port"].(type) {
		case int:
			portOK = v > 0 && v <= 65535
		case float64:
			portOK = v > 0 && v <= 65535
		case string:
			n, err := strconv.Atoi(v)
			portOK = err == nil && n > 0 && n <= 65535
		}
		if !portOK {
			skipped = append(skipped, Skip{Name: name, Reason: "missing or invalid port"})
			continue
		}
		cp := make(map[string]any, len(p))
		for k, v := range p {
			bad := false
			for _, fk := range forbiddenKeys {
				if strings.EqualFold(k, fk) {
					bad = true
					break
				}
			}
			if bad {
				continue
			}
			if k == "private-key" {
				if s, ok := v.(string); ok && (strings.HasPrefix(s, "/") || strings.HasPrefix(s, "./")) {
					bad = true
				}
			}
			if !bad {
				cp[k] = v
			}
		}
		seen[name] = true
		kept = append(kept, cp)
	}
	return kept, skipped
}

// Parse accepts Clash/Mihomo YAML, Surge [Proxy] INI, or newline/Base64 share links.
func Parse(name string, content []byte) (*Source, error) {
	if len(content) > 8<<20 {
		return nil, fmt.Errorf("configuration exceeds 8 MiB")
	}
	text := strings.TrimSpace(string(content))
	if text == "" {
		return nil, fmt.Errorf("empty proxy configuration")
	}
	var raw []map[string]any
	if strings.HasPrefix(text, "proxies:") || strings.Contains(text, "\nproxies:") || strings.HasPrefix(text, "---") || strings.Contains(text, "\nproxy-providers:") {
		var doc struct {
			Proxies        []map[string]any `yaml:"proxies"`
			ProxyProviders map[string]struct {
				Type    string           `yaml:"type"`
				Payload []map[string]any `yaml:"payload"`
			} `yaml:"proxy-providers"`
		}
		if err := yaml.Unmarshal(content, &doc); err != nil {
			return nil, fmt.Errorf("parse Clash/Mihomo YAML: %w", err)
		}
		raw = append(raw, doc.Proxies...)
		for pname, prov := range doc.ProxyProviders {
			if prov.Type != "" && prov.Type != "inline" {
				continue
			}
			for _, p := range prov.Payload {
				if _, ok := p["name"]; !ok {
					p["name"] = pname
				}
				raw = append(raw, p)
			}
		}
	} else if strings.Contains(text, "[Proxy]") {
		raw = parseSurge(text)
	} else {
		var err error
		raw, err = parseShareLinks(text)
		if err != nil {
			return nil, err
		}
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("no proxy nodes found in input")
	}
	kept, skipped := Sanitize(raw)
	if len(kept) == 0 {
		return nil, fmt.Errorf("no usable proxy nodes after validation (%d skipped)", len(skipped))
	}
	return &Source{Name: name, Proxies: kept, Skipped: skipped}, nil
}

func parseSurge(text string) []map[string]any {
	var out []map[string]any
	inProxy := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inProxy = line == "[Proxy]"
			continue
		}
		if !inProxy || line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		name, rhs, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		parts := strings.Split(rhs, ",")
		if len(parts) < 3 {
			continue
		}
		m := map[string]any{
			"name":   strings.TrimSpace(name),
			"type":   strings.ToLower(strings.TrimSpace(parts[0])),
			"server": strings.TrimSpace(parts[1]),
			"port":   strings.TrimSpace(parts[2]),
		}
		for _, opt := range parts[3:] {
			k, v, ok := strings.Cut(strings.TrimSpace(opt), "=")
			if !ok {
				continue
			}
			switch k {
			case "username":
				m["username"] = v
			case "password":
				m["password"] = v
			case "encrypt-method", "cipher":
				m["cipher"] = v
			case "tls":
				m["tls"] = v == "true"
			case "sni":
				m["sni"] = v
			case "underlying-proxy":
				m["dialer-proxy"] = strings.Trim(v, "\"")
			}
		}
		out = append(out, m)
	}
	return out
}

func parseShareLinks(text string) ([]map[string]any, error) {
	lines := strings.Split(text, "\n")
	if len(lines) <= 2 && !strings.Contains(text, "://") {
		if decoded, err := base64.StdEncoding.DecodeString(strings.Join(lines, "")); err == nil {
			lines = strings.Split(string(decoded), "\n")
		}
	}
	var out []map[string]any
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		u, err := url.Parse(line)
		if err != nil || u.Scheme == "" {
			continue
		}
		host := u.Hostname()
		port, _ := strconv.Atoi(u.Port())
		name := u.Fragment
		if name == "" {
			name = host
		}
		q := u.Query()
		switch strings.ToLower(u.Scheme) {
		case "vless", "trojan":
			m := map[string]any{
				"name": name, "type": strings.ToLower(u.Scheme), "server": host, "port": port,
				"network": defaultStr(q.Get("type"), "tcp"),
			}
			if u.Scheme == "vless" {
				m["uuid"] = u.User.Username()
				m["tls"] = q.Get("security") == "tls" || q.Get("security") == "reality"
			} else {
				m["password"] = u.User.Username()
				m["tls"] = true
			}
			if sni := q.Get("sni"); sni != "" {
				m["sni"] = sni
			}
			if fp := q.Get("fp"); fp != "" {
				m["client-fingerprint"] = fp
			}
			if flow := q.Get("flow"); flow != "" {
				m["flow"] = flow
			}
			if q.Get("security") == "reality" {
				m["reality-opts"] = map[string]any{"public-key": q.Get("pbk"), "short-id": q.Get("sid")}
			}
			if q.Get("type") == "ws" {
				m["ws-opts"] = map[string]any{"path": defaultStr(q.Get("path"), "/")}
			}
			out = append(out, m)
		case "ss":
			m := map[string]any{"name": name, "type": "ss", "server": host, "port": port}
			userinfo := u.User.String()
			if decoded, err := base64.RawURLEncoding.DecodeString(userinfo); err == nil {
				userinfo = string(decoded)
			} else if decoded, err := base64.StdEncoding.DecodeString(userinfo); err == nil {
				userinfo = string(decoded)
			}
			method, pass, ok := strings.Cut(userinfo, ":")
			if !ok {
				continue
			}
			m["cipher"], m["password"] = method, pass
			out = append(out, m)
		case "vmess":
			raw := strings.TrimPrefix(line, "vmess://")
			decoded, err := base64.StdEncoding.DecodeString(raw)
			if err != nil {
				continue
			}
			var v struct {
				PS   string `json:"ps"`
				Add  string `json:"add"`
				Port any    `json:"port"`
				ID   string `json:"id"`
				Aid  any    `json:"aid"`
				Net  string `json:"net"`
				TLS  string `json:"tls"`
				Sni  string `json:"sni"`
				Path string `json:"path"`
			}
			if json.Unmarshal(decoded, &v) != nil {
				continue
			}
			m := map[string]any{
				"name": defaultStr(v.PS, v.Add), "type": "vmess", "server": v.Add, "port": v.Port,
				"uuid": v.ID, "alterId": v.Aid, "cipher": "auto", "network": defaultStr(v.Net, "tcp"), "tls": v.TLS == "tls",
			}
			if v.Sni != "" {
				m["servername"] = v.Sni
			}
			if v.Net == "ws" {
				m["ws-opts"] = map[string]any{"path": defaultStr(v.Path, "/")}
			}
			out = append(out, m)
		case "hysteria2", "hy2", "tuic":
			typ := strings.ToLower(u.Scheme)
			if typ == "hy2" {
				typ = "hysteria2"
			}
			pass := u.User.Username()
			m := map[string]any{"name": name, "type": typ, "server": host, "port": port, "password": pass}
			if typ == "tuic" {
				m["uuid"] = pass
				if p, ok := u.User.Password(); ok {
					m["password"] = p
				}
			}
			if sni := q.Get("sni"); sni != "" {
				m["sni"] = sni
			}
			if q.Get("insecure") == "1" {
				m["skip-cert-verify"] = true
			}
			out = append(out, m)
		}
	}
	return out, nil
}

func defaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// BuildMihomoConfig renders a loopback-only, fail-closed Mihomo config for
// the gateway VM. chain lists nodes from first hop to final exit.
func BuildMihomoConfig(src *Source, selected string, chain []string, socksPort int) ([]byte, error) {
	if socksPort < 1024 || socksPort > 65535 {
		return nil, fmt.Errorf("invalid socks port %d", socksPort)
	}
	byName := map[string]map[string]any{}
	for _, p := range src.Proxies {
		byName[p["name"].(string)] = p
	}
	final := selected
	var proxies []map[string]any
	if len(chain) > 0 {
		for _, hop := range chain {
			if byName[hop] == nil {
				return nil, fmt.Errorf("chain node %q not found", hop)
			}
		}
		final = chain[len(chain)-1]
		for _, p := range src.Proxies {
			cp := make(map[string]any, len(p)+1)
			for k, v := range p {
				cp[k] = v
			}
			name := p["name"].(string)
			for i := len(chain) - 2; i >= 0; i-- {
				if chain[i+1] == name {
					cp["dialer-proxy"] = "pvm-hop-" + strconv.Itoa(i)
				}
			}
			proxies = append(proxies, cp)
		}
		for i := 0; i < len(chain)-1; i++ {
			cp := make(map[string]any, len(byName[chain[i]])+1)
			for k, v := range byName[chain[i]] {
				cp[k] = v
			}
			cp["name"] = "pvm-hop-" + strconv.Itoa(i)
			if i > 0 {
				cp["dialer-proxy"] = "pvm-hop-" + strconv.Itoa(i-1)
			}
			proxies = append(proxies, cp)
		}
	} else {
		if byName[final] == nil {
			return nil, fmt.Errorf("selected node %q not found", selected)
		}
		proxies = src.Proxies
	}
	doc := map[string]any{
		"socks-port":          socksPort,
		"bind-address":        "127.0.0.1",
		"allow-lan":           false,
		"mode":                "rules",
		"log-level":           "warning",
		"ipv6":                false,
		"external-controller": "",
		"proxies":             proxies,
		"proxy-groups": []map[string]any{
			{"name": "PVM-EXIT", "type": "select", "proxies": []string{final}},
		},
		"rules": []string{"MATCH,PVM-EXIT"},
		"dns": map[string]any{
			"enable":             true,
			"listen":             "127.0.0.1:10553",
			"ipv6":               false,
			"enhanced-mode":      "fake-ip",
			"fake-ip-range":      "198.19.0.0/16",
			"default-nameserver": []string{"1.1.1.1", "8.8.8.8"},
			"nameserver":         []string{"https://1.1.1.1/dns-query", "https://dns.google/dns-query"},
		},
	}
	return yaml.Marshal(doc)
}
