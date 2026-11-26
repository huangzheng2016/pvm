package daemon

import (
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/huangzheng2016/pvm/internal/diaglog"
)

//go:embed dashboard.html
var dashboardHTML []byte

type dashboardServer struct {
	mu    sync.Mutex
	ln    net.Listener
	token string
}

func (s *Server) handleDashboardOpen(w http.ResponseWriter, r *http.Request) {
	s.dashboard.mu.Lock()
	defer s.dashboard.mu.Unlock()
	var b [16]byte
	_, _ = rand.Read(b[:])
	s.dashboard.token = hex.EncodeToString(b[:])
	if s.dashboard.ln == nil {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		s.dashboard.ln = ln
		srv := &http.Server{Handler: s.dashboardHandler(), ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = srv.Serve(ln) }()
	}
	port := s.dashboard.ln.Addr().(*net.TCPAddr).Port
	writeJSON(w, 200, map[string]any{
		"url":   fmt.Sprintf("http://127.0.0.1:%d/#token=%s", port, s.dashboard.token),
		"port":  port,
		"scope": "read-only",
	})
}

func allowedDashboardHost(host, portStr string) bool {
	h, p, err := net.SplitHostPort(host)
	if err != nil || p != portStr {
		return false
	}
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

// dashboardHandler serves a strictly read-only view on its own loopback
// listener. It never exposes the control plane's mutation endpoints.
func (s *Server) dashboardHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(dashboardHTML)
	})
	mux.HandleFunc("GET /api/snapshot", func(w http.ResponseWriter, r *http.Request) {
		s.dashboard.mu.Lock()
		want := s.dashboard.token
		s.dashboard.mu.Unlock()
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if want == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		infos, _ := s.collectInfos()
		if infos == nil {
			infos = []vmInfo{}
		}
		routes := map[string]any{}
		if s.Clusters != nil {
			if cl := s.Clusters.Get(1); cl != nil {
				routes["rules"] = cl.Rules
				routes["vm_exits"] = cl.VMExits
				routes["default_exit"] = cl.DefaultExit
				routes["paused"] = cl.Paused
				routes["require_check"] = cl.RequireCheck
				routes["maps"] = cl.Maps
			}
		}
		payload := map[string]any{
			"clusters": s.clusterStatuses(),
			"vms":      infos,
			"routes":   routes,
		}
		raw, _ := json.Marshal(payload)
		redacted := diaglog.RedactorForPaths(s.P).Redact(string(raw))
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(redacted))
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		portStr := ""
		s.dashboard.mu.Lock()
		if s.dashboard.ln != nil {
			portStr = fmt.Sprint(s.dashboard.ln.Addr().(*net.TCPAddr).Port)
		}
		s.dashboard.mu.Unlock()
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if portStr == "" || !allowedDashboardHost(r.Host, portStr) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		mux.ServeHTTP(w, r)
	})
}
