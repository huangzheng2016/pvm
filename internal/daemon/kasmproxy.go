package daemon

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
)

// kasmProxy is a loopback HTTP reverse proxy in front of a VM's kasm
// endpoint. The kasmweb image has no switch to disable its HTTPS basic
// auth (empty VNC_PW still yields 401), so the daemon injects the
// per-container credential here: users open a bare
// http://127.0.0.1:port/ URL — no login, no self-signed cert prompt.
// Loopback-only, so anyone who can reach it is already on this machine.
type kasmProxy struct {
	port int
	srv  *http.Server
	pw   atomic.Value // string: current container VNC_PW
}

func newKasmProxy(targetPort int, pw string) (*kasmProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	target := &url.URL{Scheme: "https", Host: fmt.Sprintf("127.0.0.1:%d", targetPort)}
	p := &kasmProxy{port: ln.Addr().(*net.TCPAddr).Port}
	p.pw.Store(pw)
	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			req.SetBasicAuth("kasm_user", p.pw.Load().(string))
		},
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	p.srv = &http.Server{Handler: rp}
	go func() { _ = p.srv.Serve(ln) }()
	return p, nil
}

// kasmProxyFor returns the VM's proxy, creating it on first use and
// rotating the injected credential when the container is recreated.
func (s *Server) kasmProxyFor(vmName string, targetPort int, pw string) (*kasmProxy, error) {
	s.kasmMu.Lock()
	defer s.kasmMu.Unlock()
	if px, ok := s.kasms[vmName]; ok {
		px.pw.Store(pw)
		return px, nil
	}
	px, err := newKasmProxy(targetPort, pw)
	if err != nil {
		return nil, err
	}
	if s.kasms == nil {
		s.kasms = map[string]*kasmProxy{}
	}
	s.kasms[vmName] = px
	return px, nil
}
