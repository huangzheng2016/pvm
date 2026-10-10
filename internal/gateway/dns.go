package gateway

import (
	"log"
	"net"
	"sync"
	"time"
)

// DNSProxy forwards UDP DNS from the cluster to the slirp resolver.
// It exists so all cluster DNS terminates on the gateway; M3 replaces
// the answer path with fake-ip.
type DNSProxy struct {
	mu       sync.RWMutex
	bindIP   string
	upstream string
	conn     *net.UDPConn
}

func NewDNSProxy(bindIP, upstream string) *DNSProxy {
	p := &DNSProxy{}
	p.Update(bindIP, upstream)
	return p
}

func (p *DNSProxy) Update(bindIP, upstream string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn != nil && (p.bindIP != bindIP) {
		p.conn.Close()
		p.conn = nil
	}
	p.bindIP, p.upstream = bindIP, upstream
}

func (p *DNSProxy) Start() error {
	p.mu.RLock()
	bindIP := p.bindIP
	p.mu.RUnlock()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(bindIP), Port: 53})
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.conn = conn
	p.mu.Unlock()
	go p.serve(conn)
	return nil
}

func (p *DNSProxy) serve(conn *net.UDPConn) {
	buf := make([]byte, 4096)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		query := make([]byte, n)
		copy(query, buf[:n])
		go p.forward(conn, src, query)
	}
}

func (p *DNSProxy) forward(conn *net.UDPConn, src *net.UDPAddr, query []byte) {
	p.mu.RLock()
	upstream := p.upstream
	p.mu.RUnlock()
	up, err := net.DialTimeout("udp4", upstream+":53", 3*time.Second)
	if err != nil {
		log.Printf("dns dial: %v", err)
		return
	}
	defer up.Close()
	up.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := up.Write(query); err != nil {
		return
	}
	buf := make([]byte, 4096)
	n, err := up.Read(buf)
	if err != nil {
		log.Printf("dns upstream: %v", err)
		return
	}
	if _, err := conn.WriteToUDP(buf[:n], src); err != nil {
		log.Printf("dns reply: %v", err)
	}
}
