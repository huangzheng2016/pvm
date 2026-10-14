package warp

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/tunnel"
)

const (
	DefaultAPIBase = "https://api.cloudflareclient.com"
	apiVersion     = "v0a5641"
)

type Client struct {
	baseURL string
	http    *http.Client
}

func NewClient(baseURL string) *Client {
	if baseURL == "" {
		baseURL = DefaultAPIBase
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12},
			ForceAttemptHTTP2: false,
			Proxy:             http.ProxyFromEnvironment,
		}},
	}
}

type Registration struct {
	ID      string `json:"id"`
	Token   string `json:"token"`
	Account struct {
		License string `json:"license"`
	} `json:"account"`
	Config struct {
		Interface struct {
			Addresses struct {
				V4 string `json:"v4"`
				V6 string `json:"v6"`
			} `json:"addresses"`
		} `json:"interface"`
		Peers []struct {
			PublicKey string   `json:"public_key"`
			Endpoint  Endpoint `json:"endpoint"`
		} `json:"peers"`
	} `json:"config"`
}

type Endpoint struct {
	Host  string `json:"host"`
	V4    string `json:"v4"`
	V6    string `json:"v6"`
	Ports []int  `json:"ports"`
}

func (c *Client) Register(ctx context.Context, publicKey string) (*Registration, error) {
	body := map[string]any{
		"fcm_token":   "",
		"install_id":  "",
		"key":         publicKey,
		"locale":      "en_US",
		"model":       "PC",
		"tos":         time.Now().UTC().Format(time.RFC3339),
		"os_version":  "16.0.0",
		"key_type":    "curve25519",
		"tunnel_type": "wireguard",
	}
	var reg Registration
	if err := c.do(ctx, http.MethodPost, "/"+apiVersion+"/reg", "", body, &reg); err != nil {
		return nil, err
	}
	if len(reg.Config.Peers) == 0 {
		var device Registration
		if err := c.do(ctx, http.MethodGet, "/"+apiVersion+"/reg/"+reg.ID, reg.Token, nil, &device); err != nil {
			return nil, fmt.Errorf("get WARP device config: %w", err)
		}
		device.ID, device.Token = reg.ID, reg.Token
		if device.Account.License == "" {
			device.Account = reg.Account
		}
		reg = device
	}
	return &reg, nil
}

func (c *Client) Delete(ctx context.Context, deviceID, token string) error {
	return c.do(ctx, http.MethodDelete, "/"+apiVersion+"/reg/"+deviceID, token, nil, nil)
}

func (c *Client) do(ctx context.Context, method, path, token string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "1.1.1.1/6.38.9-5641 (Android 16.0.0)")
	req.Header.Set("CF-Client-Version", "a-6.38.9-5641")
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Cloudflare API %s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(data))
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode Cloudflare response: %w", err)
		}
	}
	return nil
}

func NewEntry(name, privateKey string, reg *Registration) (*exit.Entry, error) {
	if reg == nil || reg.ID == "" || reg.Token == "" {
		return nil, fmt.Errorf("Cloudflare response is missing device credentials")
	}
	if len(reg.Config.Peers) == 0 {
		return nil, fmt.Errorf("Cloudflare response has no WireGuard peer")
	}
	peer := reg.Config.Peers[0]
	if _, err := tunnel.Hex(peer.PublicKey); err != nil {
		return nil, fmt.Errorf("invalid Cloudflare peer public key")
	}
	publicKey, err := tunnel.PublicKey(privateKey)
	if err != nil {
		return nil, err
	}
	ipv4, err := address(reg.Config.Interface.Addresses.V4)
	if err != nil {
		return nil, fmt.Errorf("invalid Cloudflare IPv4 address: %w", err)
	}
	ipv6 := ""
	if raw := reg.Config.Interface.Addresses.V6; raw != "" {
		ipv6, err = address(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid Cloudflare IPv6 address: %w", err)
		}
	}
	endpoint, err := peer.Endpoint.String()
	if err != nil {
		return nil, err
	}
	return &exit.Entry{
		Name:         name,
		Provider:     exit.ProviderWARP,
		Server:       "api.cloudflareclient.com",
		Transport:    "udp",
		WGPrivateKey: privateKey,
		WGPublicKey:  publicKey,
		ServerPubKey: peer.PublicKey,
		TunnelIP:     ipv4,
		MTU:          1280,
		Endpoint:     endpoint,
		CreatedAt:    time.Now(),
		WARP: &exit.WARPAccount{
			DeviceID: reg.ID,
			Token:    reg.Token,
			License:  reg.Account.License,
			IPv6:     ipv6,
		},
	}, nil
}

func address(raw string) (string, error) {
	if addr, err := netip.ParseAddr(raw); err == nil {
		return addr.String(), nil
	}
	prefix, err := netip.ParsePrefix(raw)
	if err != nil {
		return "", err
	}
	return prefix.Addr().String(), nil
}

func (e Endpoint) String() (string, error) {
	host := e.Host
	if host == "" {
		host = e.V4
	}
	if host == "" {
		host = e.V6
	}
	if host == "" {
		return "", fmt.Errorf("Cloudflare response has no WireGuard endpoint")
	}
	if strings.ContainsAny(host, " \t\r\n") {
		return "", fmt.Errorf("invalid WireGuard endpoint %q", host)
	}
	if h, p, err := net.SplitHostPort(host); err == nil {
		if h == "" {
			return "", fmt.Errorf("invalid WireGuard endpoint %q", host)
		}
		port, err := strconv.ParseUint(p, 10, 16)
		if err != nil || port == 0 {
			return "", fmt.Errorf("invalid WireGuard endpoint %q", host)
		}
		return host, nil
	}
	port := 2408
	if len(e.Ports) > 0 {
		port = e.Ports[0]
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid WireGuard endpoint port %d", port)
	}
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}
