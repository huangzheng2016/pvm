package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"

	"github.com/huangzheng2016/pvm/internal/cluster"
	"github.com/huangzheng2016/pvm/internal/vm"
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
	m, code, err := s.addMap(spec, req.HostPort, req.GuestPort, req.Proto, s.pushGatewayConfig)
	if err != nil {
		writeErr(w, code, err)
		return
	}
	writeJSON(w, 201, m)
}

// addMap validates and installs a host->guest port mapping for spec,
// rolling back on relay or gateway-config failure. The status return is
// the HTTP code for API callers; the guest control path only uses err.
func (s *Server) addMap(spec *vm.Spec, hostPort, guestPort int, proto string, push func(int) error) (*cluster.PortMap, int, error) {
	if spec.Role == "gateway" || spec.ClusterID == 0 {
		return nil, 400, fmt.Errorf("vm %s is not a cluster vm", spec.Name)
	}
	if proto == "" {
		proto = "tcp"
	}
	if hostPort == 0 {
		port, err := freeLoopbackPort(proto)
		if err != nil {
			return nil, 500, err
		}
		hostPort = port
	}
	if hostPort < 1024 || hostPort > 65535 {
		return nil, 400, fmt.Errorf("host_port must be 1024-65535")
	}
	if err := portFree(proto, hostPort); err != nil {
		return nil, 409, err
	}

	m, err := s.Clusters.AddMap(spec.ClusterID, spec.Name, spec.IP, hostPort, guestPort, proto)
	if err != nil {
		return nil, 409, err
	}
	if err := s.relays.Ensure(m.Proto, m.HostPort, m.PoolPort); err != nil {
		_, _ = s.Clusters.RemoveMap(spec.ClusterID, m.HostPort, m.Proto)
		return nil, 500, err
	}
	if err := push(spec.ClusterID); err != nil {
		s.relays.Remove(m.Proto, m.HostPort)
		_, _ = s.Clusters.RemoveMap(spec.ClusterID, m.HostPort, m.Proto)
		_ = push(spec.ClusterID)
		return nil, 500, fmt.Errorf("map rolled back after gateway config push failed: %w", err)
	}
	return m, 201, nil
}

func (s *Server) handleMapRemove(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil {
		writeErr(w, 400, fmt.Errorf("bad port"))
		return
	}
	proto := r.PathValue("proto")
	previous := s.Clusters.List()
	rollback := func() {
		for _, cl := range previous {
			_ = s.Clusters.SetMaps(cl.ID, cl.Maps)
			for _, m := range cl.Maps {
				_ = s.relays.Ensure(m.Proto, m.HostPort, m.PoolPort)
			}
			_ = s.pushGatewayConfig(cl.ID)
		}
	}
	s.relays.Remove(proto, port)
	removed := false
	for _, cl := range previous {
		ok, err := s.Clusters.RemoveMap(cl.ID, port, proto)
		if err != nil {
			rollback()
			writeErr(w, 500, fmt.Errorf("map remove: %w", err))
			return
		}
		removed = removed || ok
		if err := s.pushGatewayConfig(cl.ID); err != nil {
			rollback()
			writeErr(w, 500, fmt.Errorf("map removal rolled back after gateway push failed: %w", err))
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
