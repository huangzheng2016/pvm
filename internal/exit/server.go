package exit

import (
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/huangzheng2016/pvm/internal/tunnel"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun"
)

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
	SubnetBase     int    // tunnel subnet octet, 10.<base>.0.0/16 + 10.<base+1>.0.0/16 (default 99)
	// DomainAllow lists CIDRs the domain-connect service may dial even
	// though they fall inside blocked private/link-local space.
	DomainAllow []string
	Verbose     bool

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

type Server struct {
	cfg      ServerConfig
	store    *PeerStore
	priv     string // base64 wg private key (shared by both transports)
	pub      string
	sugTZ    string // suggested client timezone (enroll response)
	nat      natBackend
	dcAllow  []*net.IPNet
	tr       map[string]*transportRT
	http     *http.Server
	ln       net.Listener
	speed    []*http.Server
	udpProbe []*net.UDPConn
	closed   chan struct{}
	once     sync.Once
}

func NewServer(cfg ServerConfig) (*Server, error) {
	cfg.defaults()
	if cfg.Token == "" {
		return nil, errors.New("--token is required")
	}
	natChoice := cfg.NAT
	if cfg.TunFactory != nil {
		// in-process tests use memory tuns + netstack
		natChoice = NATUserspace
	}
	nat, err := resolveNAT(natChoice)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, tr: map[string]*transportRT{}, nat: nat, closed: make(chan struct{})}
	s.sugTZ = DetectSuggestedTZ(cfg.StateDir)
	for _, c := range cfg.DomainAllow {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, fmt.Errorf("--domain-allow-cidr %q: %w", c, err)
		}
		s.dcAllow = append(s.dcAllow, n)
	}

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

	// speed-test endpoints, one per tunnel NIC server address
	if s.cfg.TunFactory == nil {
		for _, t := range s.tr {
			spAddr := net.JoinHostPort(t.serverIP, itoa(SpeedPort))
			if s.nat.name() == NATUserspace {
				if ub, ok := s.nat.(*userspaceBackend); ok {
					ub.Intercept(spAddr, serveSpeedConn)
				}
				continue
			}
			srv := &http.Server{Handler: speedHandler()}
			ln, err := net.Listen("tcp", spAddr)
			if err != nil {
				s.Close()
				return "", fmt.Errorf("speed endpoint %s: %w", spAddr, err)
			}
			s.speed = append(s.speed, srv)
			go srv.Serve(ln)
		}
	}

	// built-in UDP/ICMP probes, one per tunnel NIC server address: netfilter
	// backends answer on the real .1 address (ICMP is answered by the kernel),
	// the userspace backend intercepts both inside its stack.
	if s.cfg.TunFactory == nil {
		for _, t := range s.tr {
			probeAddr := net.JoinHostPort(t.serverIP, itoa(UDPProbePort))
			if s.nat.name() == NATUserspace {
				if ub, ok := s.nat.(*userspaceBackend); ok {
					ub.InterceptUDP(probeAddr, serveUDPProbeConn)
					ub.AnswerICMPLocal(t.serverIP)
				}
				continue
			}
			ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(t.serverIP), Port: UDPProbePort})
			if err != nil {
				s.Close()
				return "", fmt.Errorf("udp probe endpoint %s: %w", probeAddr, err)
			}
			s.udpProbe = append(s.udpProbe, ln)
			go udpProbeListener(ln)
		}
	}

	// domain-connect listeners, one per tunnel NIC server address
	dc := newDCServer(s.dcAllow)
	for _, t := range s.tr {
		dcAddr := net.JoinHostPort(t.serverIP, itoa(DomainConnectPort))
		if s.nat.name() == NATUserspace {
			// no OS address exists; the netstack forwarder intercepts it
			if ub, ok := s.nat.(*userspaceBackend); ok {
				ub.Intercept(dcAddr, dc.handle)
			}
			continue
		}
		ln, err := net.Listen("tcp", dcAddr)
		if err != nil {
			s.Close()
			return "", fmt.Errorf("domain-connect %s: %w", dcAddr, err)
		}
		go dc.serve(ln)
	}
	log.Printf("domain-connect: listening on each tunnel NIC (port %d)", DomainConnectPort)

	// drop registrations whose tunnel has been silent for too long,
	// otherwise the /16 pool slowly fills with dead peers.
	go s.reapIdlePeers()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("POST /v1/enroll", s.withAuth(s.handleEnroll))
	mux.HandleFunc("DELETE /v1/peers/{ip}", s.withAuth(s.handleDeletePeer))
	mux.HandleFunc("GET /v1/peers", s.withAuth(s.handleListPeers))
	s.http = &http.Server{
		Addr:    s.cfg.Listen,
		Handler: mux,
		// the control plane listens on the public internet; without
		// timeouts a slowloris trickle pins one goroutine+fd per conn.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       60 * time.Second,
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
	subnet, err := subnetOf(s.store.Subnets, name)
	if err != nil {
		return err
	}
	serverIP, err := serverIPOf(s.store.Subnets, name)
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
	want := "Bearer " + s.cfg.Token
	return func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(auth), []byte(want)) != 1 {
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
	ServerIP     string `json:"server_ip"` // tunnel-side .1 address (speed/ping endpoint)
	MTU          int    `json:"mtu"`
	Endpoint     string `json:"endpoint"`
	// SuggestedTZ is the exit's public-geo timezone (IANA), detected at
	// agent startup; empty means "treat as UTC".
	SuggestedTZ string `json:"suggested_tz,omitempty"`
}

func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	var req enrollRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&req); err != nil {
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
	peer, created, err := s.store.Allocate(req.PubKey, req.Hostname, req.Transport)
	if err != nil {
		http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusInternalServerError)
		return
	}
	pubHex, _ := tunnel.Hex(req.PubKey)
	if err := t.dev.IpcSet(fmt.Sprintf("public_key=%s\nallowed_ip=%s/32\n", pubHex, peer.IP)); err != nil {
		// only roll back peers this call actually created; removing an
		// existing registration would desync store and device.
		if created {
			_, _, _ = s.store.Remove(peer.IP)
		}
		http.Error(w, `{"error":"configure peer"}`, http.StatusInternalServerError)
		return
	}
	writeJSON(w, enrollResponse{
		ServerPubKey: s.pub,
		TunnelIP:     peer.IP,
		ServerIP:     t.serverIP,
		MTU:          MTUFor(req.Transport),
		Endpoint:     s.publicEndpoint(r, t),
		SuggestedTZ:  s.sugTZ,
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
	peer, ok := s.store.Get(ip)
	if !ok {
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		return
	}
	// device first: only when the key is actually off the wireguard device
	// is it safe to drop the registration; otherwise the peer stays
	// connectable while the API claims it is gone.
	if t, ok := s.tr[peer.Transport]; ok {
		pubHex, err := tunnel.Hex(peer.PubKey)
		if err != nil {
			http.Error(w, `{"error":"device remove peer"}`, http.StatusInternalServerError)
			return
		}
		if err := t.dev.IpcSet(fmt.Sprintf("public_key=%s\nremove=true\n", pubHex)); err != nil {
			http.Error(w, `{"error":"device remove peer"}`, http.StatusInternalServerError)
			return
		}
	}
	if _, _, err := s.store.Remove(ip); err != nil {
		http.Error(w, `{"error":"store"}`, http.StatusInternalServerError)
		return
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

func (s *Server) ControlAddr() string {
	if s.ln == nil {
		return ""
	}
	return s.ln.Addr().String()
}

func (s *Server) Close() {
	s.once.Do(func() {
		close(s.closed)
		if s.http != nil {
			_ = s.http.Close()
		}
		for _, srv := range s.speed {
			_ = srv.Close()
		}
		for _, ln := range s.udpProbe {
			_ = ln.Close()
		}
		for _, t := range s.tr {
			t.dev.Close()
		}
		if s.cfg.TunFactory == nil {
			var ifs, cidrs []string
			for _, t := range s.tr {
				ifs = append(ifs, t.tunName)
				cidrs = append(cidrs, t.subnet)
			}
			_ = s.nat.cleanup(ifs, cidrs)
		}
	})
}

const (
	peerReapInterval = time.Hour
	peerIdleMax      = 24 * time.Hour
)

// reapIdlePeers periodically removes registrations whose tunnel has been
// silent for peerIdleMax (or that never handshook within that window), so
// the /16 tunnel-IP pool cannot fill with dead peers over time.
func (s *Server) reapIdlePeers() {
	tick := time.NewTicker(peerReapInterval)
	defer tick.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-tick.C:
			s.reapIdleOnce()
		}
	}
}

func (s *Server) reapIdleOnce() {
	for _, p := range s.store.List() {
		t, ok := s.tr[p.Transport]
		if !ok {
			continue
		}
		hs, err := peerLastHandshake(t.dev, p.PubKey)
		if err != nil {
			continue
		}
		idle := time.Since(hs)
		if hs.IsZero() {
			idle = time.Since(p.CreatedAt)
		}
		if idle < peerIdleMax {
			continue
		}
		pubHex, err := tunnel.Hex(p.PubKey)
		if err != nil {
			continue
		}
		if err := t.dev.IpcSet(fmt.Sprintf("public_key=%s\nremove=true\n", pubHex)); err != nil {
			log.Printf("reap peer %s: device remove: %v", p.IP, err)
			continue
		}
		if _, _, err := s.store.Remove(p.IP); err == nil {
			log.Printf("reaped idle peer %s (%s, idle %v)", p.IP, p.Hostname, idle.Round(time.Minute))
		}
	}
}

// peerLastHandshake parses the device's IpcGet dump for one peer's
// last_handshake_time_sec; zero time means never handshook / absent.
func peerLastHandshake(dev *device.Device, pubkey string) (time.Time, error) {
	out, err := dev.IpcGet()
	if err != nil {
		return time.Time{}, err
	}
	wantHex, err := tunnel.Hex(pubkey)
	if err != nil {
		return time.Time{}, err
	}
	return parseIpcLastHandshake(out, wantHex), nil
}

// parseIpcLastHandshake finds last_handshake_time_sec for wantHex in an
// IpcGet dump; zero when the peer is absent or never shook hands.
func parseIpcLastHandshake(out, wantHex string) time.Time {
	cur := ""
	for _, line := range strings.Split(out, "\n") {
		if k, ok := strings.CutPrefix(line, "public_key="); ok {
			cur = k
			continue
		}
		if cur != wantHex {
			continue
		}
		if v, ok := strings.CutPrefix(line, "last_handshake_time_sec="); ok {
			sec, _ := strconv.ParseInt(v, 10, 64)
			if sec <= 0 {
				return time.Time{}
			}
			return time.Unix(sec, 0)
		}
	}
	return time.Time{}
}
