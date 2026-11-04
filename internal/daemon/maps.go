package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

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
	s.mapMu.Lock()
	defer s.mapMu.Unlock()
	m, code, err := s.addMap(r.Context(), spec, req.HostPort, req.GuestPort, req.Proto, s.pushGatewayConfigCtx)
	if err != nil {
		writeErr(w, code, err)
		return
	}
	writeJSON(w, 201, m)
}

// rollbackPushTimeout bounds restore/cleanup pushes that run inside the
// mapMu critical section: they must complete even when the caller's
// context is done, but a hung ssh must not hold the lock forever. It
// is a variable so tests can shorten it.
var rollbackPushTimeout = 30 * time.Second

// rollbackPush re-pushes restored state with its own bounded context,
// independent of the (possibly cancelled) caller context.
func rollbackPush(push func(context.Context, int) error, clusterID int) error {
	ctx, cancel := context.WithTimeout(context.Background(), rollbackPushTimeout)
	defer cancel()
	return push(ctx, clusterID)
}

// pushBounded runs a gateway config push with its own timeout so a hung
// ssh cannot hold mapMu forever. For call sites without an injectable
// push function.
func (s *Server) pushBounded(clusterID int) error {
	ctx, cancel := context.WithTimeout(context.Background(), rollbackPushTimeout)
	defer cancel()
	return s.pushGatewayConfigCtx(ctx, clusterID)
}

// addMap validates and installs a host->guest port mapping for spec,
// rolling back on relay or gateway-config failure. The status return is
// the HTTP code for API callers; the guest control path only uses err.
// Callers hold mapMu: the global port/proto ownership check and the
// mutation must be atomic against every other map add/remove/rollback.
func (s *Server) addMap(ctx context.Context, spec *vm.Spec, hostPort, guestPort int, proto string, push func(context.Context, int) error) (*cluster.PortMap, int, error) {
	if spec.Role == "gateway" || spec.ClusterID == 0 {
		return nil, 400, fmt.Errorf("vm %s is not a cluster vm", spec.Name)
	}
	// The VM must still be a cluster member: a delete/rollback that ran
	// after the caller loaded the spec must not be re-entered by a new
	// map for a removed VM.
	if cl := s.Clusters.Get(spec.ClusterID); cl == nil || cl.VMs[spec.Name] == nil {
		return nil, 404, fmt.Errorf("vm %s is not a cluster member", spec.Name)
	}
	if proto == "" {
		proto = "tcp"
	}
	if hostPort == 0 {
		port, err := s.freeMapPort(proto)
		if err != nil {
			return nil, 500, err
		}
		hostPort = port
	}
	if hostPort < 1024 || hostPort > 65535 {
		return nil, 400, fmt.Errorf("host_port must be 1024-65535")
	}
	// Ownership is global: a port persisted by another cluster without
	// a live relay is still taken and must not be hijackable.
	if s.mapPortInUse(hostPort, proto) {
		return nil, 409, fmt.Errorf("host port %d/%s already mapped", hostPort, proto)
	}
	if err := portFree(proto, hostPort); err != nil {
		return nil, 409, err
	}
	if err := ctx.Err(); err != nil {
		return nil, 500, err
	}

	m, err := s.Clusters.AddMap(spec.ClusterID, spec.Name, spec.IP, hostPort, guestPort, proto)
	if err != nil {
		return nil, 409, err
	}
	if err := s.relays.Ensure(m.Proto, m.HostPort, m.PoolPort); err != nil {
		_, _ = s.Clusters.RemoveMap(spec.ClusterID, m.HostPort, m.Proto)
		return nil, 500, err
	}
	if err := ctx.Err(); err != nil {
		s.relays.Remove(m.Proto, m.HostPort)
		_, _ = s.Clusters.RemoveMap(spec.ClusterID, m.HostPort, m.Proto)
		return nil, 500, err
	}
	if err := push(ctx, spec.ClusterID); err != nil {
		s.relays.Remove(m.Proto, m.HostPort)
		_, _ = s.Clusters.RemoveMap(spec.ClusterID, m.HostPort, m.Proto)
		_ = rollbackPush(push, spec.ClusterID)
		return nil, 500, fmt.Errorf("map rolled back after gateway config push failed: %w", err)
	}
	return m, 201, nil
}

// mapPortInUse reports whether any cluster already persists hostPort
// with proto, regardless of whether its relay is currently bound.
func (s *Server) mapPortInUse(hostPort int, proto string) bool {
	for _, cl := range s.Clusters.List() {
		for _, m := range cl.Maps {
			if m.HostPort == hostPort && m.Proto == proto {
				return true
			}
		}
	}
	return false
}

// freeMapPort picks a loopback port that is both bindable and not
// persisted by any cluster.
func (s *Server) freeMapPort(proto string) (int, error) {
	for p := 18080; p < 18080+2000; p++ {
		if s.mapPortInUse(p, proto) {
			continue
		}
		if portFree(proto, p) == nil {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free loopback port")
}

func (s *Server) handleMapRemove(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil {
		writeErr(w, 400, fmt.Errorf("bad port"))
		return
	}
	proto := r.PathValue("proto")
	s.mapMu.Lock()
	defer s.mapMu.Unlock()
	previous := s.Clusters.List()
	rollback := func() {
		for _, cl := range previous {
			_ = s.Clusters.SetMaps(cl.ID, cl.Maps)
			for _, m := range cl.Maps {
				_ = s.relays.Ensure(m.Proto, m.HostPort, m.PoolPort)
			}
			_ = s.pushBounded(cl.ID)
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
		if err := s.pushGatewayConfigCtx(r.Context(), cl.ID); err != nil {
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
