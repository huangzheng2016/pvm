package exit

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/huangzheng2016/pvm/internal/tunnel"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
)

// ServerConfig controls the exit-agent server.
type ServerConfig struct {
	Listen         string // control plane TLS listen, e.g. :18443
	Token          string
	StateDir       string // cert, wg key, peers.json
	PublicEndpoint string // host[:port] override announced to clients
	UDPPort        int    // WireGuard-over-UDP port
	TCPPort        int    // WireGuard-over-TCP port
	Transports     string // both | udp | tcp
	NAT            string // auto | nft | iptables | userspace
	TunPrefix      string // OS TUN name prefix (default "pvmexit")
	SubnetBase     int    // tunnel subnet octet, 10.<base>.0/1.0/24 (default 99)
	Verbose        bool

	// TunFactory overrides OS TUN creation (tests); when set, system
	// network configuration (ip/nft/sysctl) is skipped.
	TunFactory func(name string, mtu int) (tun.Device, error)
}

func (c *ServerConfig) defaults() {
	if c.Listen == "" {
		c.Listen = ":18443"
	}
	if c.StateDir == "" {
		c.StateDir = "/var/lib/pvm-exit"
	}
	if c.UDPPort == 0 {
		c.UDPPort = 18443
	}
	if c.TCPPort == 0 {
		c.TCPPort = 18444
	}
	if c.Transports == "" {
		c.Transports = "both"
	}
}

// MTUFor picks the interface/peer MTU per transport; TCP framing and
// TCP-over-TCP dynamics call for a smaller MTU.
func MTUFor(transport string) int {
	if transport == "tcp" {
		return 1380
	}
	return 1420
}

type transportRT struct {
	name       string
	dev        *device.Device
	tunName    string
	subnet     string
	serverIP   string
	port       int
	publicPort int
}

// Server is the VPS-side exit agent.
type Server struct {
	cfg    ServerConfig
	store  *PeerStore
	priv   string // base64 wg private key (shared by both transports)
	pub    string
	nat    natBackend
	tr     map[string]*transportRT
	http   *http.Server
	ln     net.Listener
	closed chan struct{}
	once   sync.Once
}

func NewServer(cfg ServerConfig) (*Server, error) {
	cfg.defaults()
	if cfg.Token == "" {
		return nil, errors.New("--token is required")
	}
	nat, err := resolveNAT(cfg.NAT)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, tr: map[string]*transportRT{}, nat: nat, closed: make(chan struct{})}

	base := cfg.SubnetBase
	if base == 0 {
		base = defaultSubnetBase
	}
	s.store, err = LoadPeerStoreWithBase(filepath.Join(cfg.StateDir, "peers.json"), base)
	if err != nil {
		return nil, err
	}
	if s.priv, err = loadOrCreateWGKey(filepath.Join(cfg.StateDir, "wg.key")); err != nil {
		return nil, err
	}
	if s.pub, err = tunnel.PublicKey(s.priv); err != nil {
		return nil, err
	}
	return s, nil
}

func loadOrCreateWGKey(path string) (string, error) {
	if b, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	k, err := tunnel.GenerateKey()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(k+"\n"), 0o600); err != nil {
		return "", err
	}
	return k, nil
}

// Start brings up the data plane transports and the TLS control plane.
// Returns the certificate fingerprint clients should TOFU-pin.
func (s *Server) Start() (string, error) {
	cert, fp, err := EnsureCert(s.cfg.StateDir)
	if err != nil {
		return "", fmt.Errorf("cert: %w", err)
	}

	want := map[string]bool{"udp": false, "tcp": false}
	switch s.cfg.Transports {
	case "both":
		want["udp"], want["tcp"] = true, true
	case "udp":
		want["udp"] = true
	case "tcp":
		want["tcp"] = true
	default:
		return "", fmt.Errorf("unknown transports %q", s.cfg.Transports)
	}

	for name := range want {
		if !want[name] {
			continue
		}
		if err := s.startTransport(name); err != nil {
			s.Close()
			return "", fmt.Errorf("transport %s: %w", name, err)
		}
	}
	// replay persisted peers into fresh devices (they do not survive restart)
	for _, p := range s.store.List() {
		t, ok := s.tr[p.Transport]
		if !ok {
			continue
		}
		pubHex, err := tunnel.Hex(p.PubKey)
		if err != nil {
			continue
		}
		if err := t.dev.IpcSet(fmt.Sprintf("public_key=%s\nallowed_ip=%s/32\n", pubHex, p.IP)); err != nil {
			log.Printf("warning: restore peer %s: %v", p.IP, err)
		}
	}

	if s.cfg.TunFactory == nil {
		var ifs, cidrs []string
		for _, t := range s.tr {
			ifs = append(ifs, t.tunName)
			cidrs = append(cidrs, t.subnet)
		}
		if err := s.nat.setup(ifs, cidrs); err != nil {
			s.Close()
			return "", fmt.Errorf("nat %s: %w", s.nat.name(), err)
		}
	}
	log.Printf("nat backend: %s", s.nat.name())

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("POST /v1/enroll", s.withAuth(s.handleEnroll))
	mux.HandleFunc("DELETE /v1/peers/{ip}", s.withAuth(s.handleDeletePeer))
	mux.HandleFunc("GET /v1/peers", s.withAuth(s.handleListPeers))
	s.http = &http.Server{
		Addr:    s.cfg.Listen,
		Handler: mux,
		TLSConfig: &tls.Config{
			Certificates: []tls.Certificate{cert},
			MinVersion:   tls.VersionTLS12,
		},
	}
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		s.Close()
		return "", err
	}
	s.ln = ln
	go func() {
		if err := s.http.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("control plane: %v", err)
		}
	}()
	return fp, nil
}

func (s *Server) startTransport(name string) error {
	subnet, err := subnetOf(s.store.subnets, name)
	if err != nil {
		return err
	}
	serverIP, err := serverIPOf(s.store.subnets, name)
	if err != nil {
		return err
	}
	port := s.cfg.UDPPort
	if name == "tcp" {
		port = s.cfg.TCPPort
	}
	mtu := MTUFor(name)
	prefix := s.cfg.TunPrefix
	if prefix == "" {
		prefix = "pvmexit"
	}
	tunName := prefix + "0"
	if name == "tcp" {
		tunName = prefix + "1"
	}

	var td tun.Device
	switch {
	case s.cfg.TunFactory != nil:
		td, err = s.cfg.TunFactory(tunName, mtu)
	default:
		td, err = s.nat.newTun(tunName, mtu)
	}
	if err != nil {
		return fmt.Errorf("create tun: %w", err)
	}
	gotName, _ := td.Name()

	dev, err := tunnel.NewDevice(td, tunnel.BindFor(name), s.cfg.Verbose)
	if err != nil {
		td.Close()
		return err
	}
	privHex, _ := tunnel.Hex(s.priv)
	if err := dev.IpcSet(fmt.Sprintf("private_key=%s\nlisten_port=%d\n", privHex, port)); err != nil {
		dev.Close()
		return fmt.Errorf("configure device: %w", err)
	}

	if s.cfg.TunFactory == nil && s.nat.name() != NATUserspace {
		if err := ConfigureTUN(gotName, serverIP+"/"+strings.Split(subnet, "/")[1]); err != nil {
			dev.Close()
			return err
		}
	}
	s.tr[name] = &transportRT{name: name, dev: dev, tunName: gotName, subnet: subnet, serverIP: serverIP, port: port, publicPort: port}
	return nil
}

func (s *Server) withAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+s.cfg.Token {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"ok": true, "version": "1", "nat": s.nat.name()})
}

type enrollRequest struct {
	PubKey    string `json:"wg_pubkey"`
	Hostname  string `json:"hostname"`
	Transport string `json:"transport"`
}

type enrollResponse struct {
	ServerPubKey string `json:"server_pubkey"`
	TunnelIP     string `json:"tunnel_ip"`
	MTU          int    `json:"mtu"`
	Endpoint     string `json:"endpoint"`
}

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req enrollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"bad json"}`, http.StatusBadRequest)
		return
	}
	if req.Transport == "" {
		req.Transport = "udp"
	}
	t, ok := s.tr[req.Transport]
	if !ok {
		http.Error(w, fmt.Sprintf(`{"error":"transport %q not enabled"}`, req.Transport), http.StatusBadRequest)
		return
	}
	if _, err := tunnel.PublicKey(req.PubKey); err != nil {
		http.Error(w, `{"error":"bad wg_pubkey"}`, http.StatusBadRequest)
		return
	}
	peer, err := s.store.Allocate(req.PubKey, req.Hostname, req.Transport)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	pubHex, _ := tunnel.Hex(req.PubKey)
	if err := t.dev.IpcSet(fmt.Sprintf("public_key=%s\nallowed_ip=%s/32\n", pubHex, peer.IP)); err != nil {
		s.store.Remove(peer.IP)
		http.Error(w, `{"error":"configure peer"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, enrollResponse{
		ServerPubKey: s.pub,
		TunnelIP:     peer.IP,
		MTU:          MTUFor(req.Transport),
		Endpoint:     s.publicEndpoint(r, t),
	})
}

func (s *Server) publicEndpoint(r *http.Request, t *transportRT) string {
	if s.cfg.PublicEndpoint != "" {
		host := s.cfg.PublicEndpoint
		if _, _, err := net.SplitHostPort(host); err != nil {
			host = net.JoinHostPort(host, itoa(t.publicPort))
		}
		return host
	}
	host, _, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
	}
	return net.JoinHostPort(host, itoa(t.publicPort))
}

func itoa(i int) string {
	return fmt.Sprintf("%d", i)
}

func (s *Server) handleDeletePeer(w http.ResponseWriter, r *http.Request) {
	ip := r.PathValue("ip")
	peer, ok, err := s.store.Remove(ip)
	if err != nil {
		http.Error(w, `{"error":"store"}`, http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	if t, ok := s.tr[peer.Transport]; ok {
		pubHex, _ := tunnel.Hex(peer.PubKey)
		_ = t.dev.IpcSet(fmt.Sprintf("public_key=%s\nremove=true\n", pubHex))
	}
	writeJSON(w, map[string]any{"removed": ip})
}

func (s *Server) handleListPeers(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"peers": s.store.List()})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// ControlAddr returns the bound control-plane address (e.g. 127.0.0.1:port).
func (s *Server) ControlAddr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

// Close tears down the control plane, devices, and the NAT backend.
func (s *Server) Close() {
	s.once.Do(func() {
		close(s.closed)
		if s.http != nil {
			_ = s.http.Close()
		}
		for _, t := range s.tr {
			t.dev.Close()
		}
		if s.cfg.TunFactory == nil {
			_ = s.nat.cleanup()
		}
	})
}
