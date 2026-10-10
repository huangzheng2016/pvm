package exit

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client talks to an exit-agent control plane with TOFU cert pinning.
type Client struct {
	Server      string // host:port of the TLS control plane
	Token       string
	Fingerprint string // pinned sha256 hex of the server cert (empty = skip pin)
}

func NewClient(server, token, fingerprint string) *Client {
	return &Client{Server: server, Token: token, Fingerprint: fingerprint}
}

// ProbeFingerprint performs a bare TLS handshake and returns the peer cert
// fingerprint. Used for first-contact TOFU.
func ProbeFingerprint(server string) (string, error) {
	var fp string
	conn, err := tls.Dial("tcp", server, &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			var err error
			fp, err = Fingerprint(rawCerts[0])
			return err
		},
	})
	if err != nil {
		return "", err
	}
	conn.Close()
	if fp == "" {
		return "", fmt.Errorf("no peer certificate")
	}
	return fp, nil
}

func (c *Client) tlsClient() *http.Client {
	expect := c.Fingerprint
	return &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec
				VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
					if expect == "" {
						return nil
					}
					fp, err := Fingerprint(rawCerts[0])
					if err != nil {
						return err
					}
					if fp != expect {
						return fmt.Errorf("server certificate fingerprint mismatch: got %s, want %s (possible MITM)", fp, expect)
					}
					return nil
				},
			},
		},
	}
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+c.Server+path, rdr)
	if err != nil {
		return err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.tlsClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(b))
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

// Health checks the unauthenticated health endpoint.
func (c *Client) Health(ctx context.Context) error {
	var v map[string]any
	return c.do(ctx, "GET", "/v1/health", nil, &v)
}

// Enroll registers our public key and returns the assigned tunnel config.
func (c *Client) Enroll(ctx context.Context, pubkey, hostname, transport string) (*enrollResponse, error) {
	var resp enrollResponse
	err := c.do(ctx, "POST", "/v1/enroll", enrollRequest{PubKey: pubkey, Hostname: hostname, Transport: transport}, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

// DeletePeer revokes the peer owning tunnelIP.
func (c *Client) DeletePeer(ctx context.Context, tunnelIP string) error {
	return c.do(ctx, "DELETE", "/v1/peers/"+tunnelIP, nil, nil)
}

// ListPeers returns enrolled peers (admin view).
func (c *Client) ListPeers(ctx context.Context) ([]Peer, error) {
	var v struct {
		Peers []Peer `json:"peers"`
	}
	if err := c.do(ctx, "GET", "/v1/peers", nil, &v); err != nil {
		return nil, err
	}
	return v.Peers, nil
}
