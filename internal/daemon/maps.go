package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"

	"github.com/huangzheng2016/pvm/internal/cluster"
)

type mapRequest struct {
	VM        string `json:"vm"`
	HostPort  int    `json:"host_port"` // 0 = auto
	GuestPort int    `json:"guest_port"`
	Proto     string `json:"proto"`
}

func (s *Server) handleMapList(w http.ResponseWriter, r *http.Request) {
	var out []*cluster.PortMap
	for _, cl := range s.Clusters.List() {
		out = append(out, cl.Maps...)
	}
	if out == nil {
		out = []*cluster.PortMap{}
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleMapAdd(w http.ResponseWriter, r *http.Request) {
	var req mapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if req.VM == "" || req.GuestPort <= 0 || req.GuestPort > 65535 {
		writeErr(w, 400, fmt.Errorf("vm and guest_port are required"))
		return
	}
	spec, err := s.getSpec(req.VM)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	if spec.Role == "gateway" || spec.ClusterID == 0 {
		writeErr(w, 400, fmt.Errorf("vm %s is not a cluster vm", req.VM))
		return
	}
	if req.Proto == "" {
		req.Proto = "tcp"
	}
	if req.HostPort == 0 {
		port, err := freeLoopbackPort(req.Proto)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		req.HostPort = port
	}
	if req.HostPort < 1024 || req.HostPort > 65535 {
		writeErr(w, 400, fmt.Errorf("host_port must be 1024-65535"))
		return
	}
	if err := portFree(req.Proto, req.HostPort); err != nil {
		writeErr(w, 409, err)
		return
	}

	m, err := s.Clusters.AddMap(spec.ClusterID, spec.Name, spec.IP, req.HostPort, req.GuestPort, req.Proto)
	if err != nil {
		writeErr(w, 409, err)
		return
	}
	if err := s.relays.Ensure(m.Proto, m.HostPort, m.PoolPort); err != nil {
		_, _ = s.Clusters.RemoveMap(spec.ClusterID, m.HostPort, m.Proto)
		writeErr(w, 500, err)
		return
	}
	if err := s.pushGatewayConfig(spec.ClusterID); err != nil {
		writeErr(w, 500, fmt.Errorf("map recorded but gateway config push failed: %w", err))
		return
	}
	writeJSON(w, 201, m)
}

func (s *Server) handleMapRemove(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil {
		writeErr(w, 400, fmt.Errorf("bad port"))
		return
	}
	proto := r.PathValue("proto")
	s.relays.Remove(proto, port)
	removed := false
	for _, cl := range s.Clusters.List() {
		ok, err := s.Clusters.RemoveMap(cl.ID, port, proto)
		if err != nil {
			writeErr(w, 500, fmt.Errorf("map remove: %w", err))
			return
		}
		removed = removed || ok
		if err := s.pushGatewayConfig(cl.ID); err != nil {
			writeErr(w, 500, fmt.Errorf("map removed but gateway push failed: %w", err))
			return
		}
	}
	if !removed {
		writeErr(w, 404, fmt.Errorf("no %s map on port %d", proto, port))
		return
	}
	writeJSON(w, 200, map[string]any{"removed": port})
}

func freeLoopbackPort(proto string) (int, error) {
	for p := 18080; p < 18080+2000; p++ {
		if portFree(proto, p) == nil {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free loopback port")
}

func portFree(proto string, port int) error {
	network := "tcp"
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	if proto == "udp" {
		network = "udp"
		c, err := net.ListenPacket(network, addr)
		if err != nil {
			return fmt.Errorf("127.0.0.1:%d/%s unavailable: %w", port, proto, err)
		}
		c.Close()
		return nil
	}
	l, err := net.Listen(network, addr)
	if err != nil {
		return fmt.Errorf("127.0.0.1:%d/%s unavailable: %w", port, proto, err)
	}
	l.Close()
	return nil
}
