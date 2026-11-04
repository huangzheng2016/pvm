package daemon

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/huangzheng2016/pvm/internal/agent"
	"github.com/huangzheng2016/pvm/internal/cluster"
	"github.com/huangzheng2016/pvm/internal/vm"
)

// guestControlHandler answers control requests from the guest on the
// given VM's agent channel. The VM identity comes from the channel
// itself; request payloads never name a VM, so a guest can only ever
// manage its own mappings.
func (s *Server) guestControlHandler(vmName string) agent.ControlHandler {
	return s.guestControlHandlerWithPush(vmName, s.pushGatewayConfig)
}

func (s *Server) guestControlHandlerWithPush(vmName string, push func(int) error) agent.ControlHandler {
	return func(req *agent.Frame) ([]byte, error) {
		switch req.Action {
		case agent.ControlMapList:
			return s.guestMapList(vmName)
		case agent.ControlMapAdd:
			return s.guestMapAdd(vmName, req.Payload, push)
		case agent.ControlMapRemove:
			return s.guestMapRemove(vmName, req.Payload, push)
		}
		return nil, fmt.Errorf("unknown control action %q", req.Action)
	}
}

func (s *Server) guestMapList(vmName string) ([]byte, error) {
	resp := agent.MapListResponse{Maps: []agent.MapEntry{}}
	for _, cl := range s.Clusters.List() {
		for _, m := range cl.Maps {
			if m.VM == vmName {
				resp.Maps = append(resp.Maps, mapEntry(m))
			}
		}
	}
	return json.Marshal(resp)
}

func (s *Server) guestMapAdd(vmName string, payload []byte, push func(int) error) ([]byte, error) {
	var req agent.MapAddRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("bad map.add payload: %v", err)
	}
	if req.GuestPort <= 0 || req.GuestPort > 65535 {
		return nil, fmt.Errorf("guest_port must be 1-65535")
	}
	spec, err := s.getSpec(vmName)
	if err != nil {
		return nil, err
	}
	m, _, err := s.addMap(spec, req.HostPort, req.GuestPort, req.Proto, push)
	if err != nil {
		return nil, err
	}
	return json.Marshal(agent.MapAddResponse{Map: mapEntry(m)})
}

// guestMapRemove deletes a mapping only when it belongs to the calling
// VM; the global host API's remove-by-port semantics are deliberately
// not reachable from the guest.
func (s *Server) guestMapRemove(vmName string, payload []byte, push func(int) error) ([]byte, error) {
	var req agent.MapRemoveRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return nil, fmt.Errorf("bad map.remove payload: %v", err)
	}
	if req.HostPort <= 0 || req.HostPort > 65535 {
		return nil, fmt.Errorf("host_port must be 1-65535")
	}
	proto := req.Proto
	if proto == "" {
		proto = "tcp"
	}

	var owner *cluster.Cluster
	var owned *cluster.PortMap
	for _, cl := range s.Clusters.List() {
		for _, m := range cl.Maps {
			if m.HostPort == req.HostPort && m.Proto == proto {
				if m.VM != vmName {
					return nil, fmt.Errorf("port %d/%s is not mapped by this vm", req.HostPort, proto)
				}
				owner, owned = cl, m
			}
		}
	}
	if owned == nil {
		return nil, fmt.Errorf("no %s map on port %d", proto, req.HostPort)
	}

	previous := owner.Maps
	s.relays.Remove(proto, req.HostPort)
	removed, err := s.Clusters.RemoveMap(owner.ID, req.HostPort, proto)
	if err != nil {
		s.restoreMaps(owner.ID, previous)
		return nil, fmt.Errorf("map remove: %v", err)
	}
	if !removed {
		s.restoreMaps(owner.ID, previous)
		return nil, fmt.Errorf("no %s map on port %d", proto, req.HostPort)
	}
	if err := push(owner.ID); err != nil {
		s.restoreMaps(owner.ID, previous)
		return nil, fmt.Errorf("map removal rolled back after gateway push failed: %v", err)
	}
	return json.Marshal(struct{}{})
}

// restoreMaps puts a cluster's maps back after a failed remove and
// re-establishes their relays.
func (s *Server) restoreMaps(clusterID int, maps []*cluster.PortMap) {
	if err := s.Clusters.SetMaps(clusterID, maps); err != nil {
		log.Printf("guest control: restore cluster %d maps: %v", clusterID, err)
		return
	}
	for _, m := range maps {
		_ = s.relays.Ensure(m.Proto, m.HostPort, m.PoolPort)
	}
}

func mapEntry(m *cluster.PortMap) agent.MapEntry {
	return agent.MapEntry{HostPort: m.HostPort, GuestPort: m.GuestPort, Proto: m.Proto}
}

// ensureControlWatcher starts the per-VM control reconnect watcher for
// the VM's current watch generation, if that generation has no watcher
// yet. A VM restart bumps the generation, so restarts never stack
// watchers.
func (s *Server) ensureControlWatcher(spec *vm.Spec) {
	seq := s.currentWatchSeq(spec.Name)
	s.ctrlWatchMu.Lock()
	if s.ctrlWatch == nil {
		s.ctrlWatch = map[string]uint64{}
	}
	if s.ctrlWatch[spec.Name] == seq {
		s.ctrlWatchMu.Unlock()
		return
	}
	s.ctrlWatch[spec.Name] = seq
	s.ctrlWatchMu.Unlock()
	go s.controlWatchLoop(spec, seq)
}

// controlWatchLoop keeps one agent connection alive per running VM so
// the guest can initiate scoped control requests at any time. It exits
// when a newer start supersedes the generation or the VM stops; the
// shared per-VM client means exec and control ride the same connection.
func (s *Server) controlWatchLoop(spec *vm.Spec, seq uint64) {
	backoff := 500 * time.Millisecond
	for {
		if s.currentWatchSeq(spec.Name) != seq {
			return
		}
		if st, err := s.instance(spec).Running(); err != nil || st == nil {
			return
		}
		ac, err := s.agentClient(spec.Name)
		if err != nil {
			time.Sleep(backoff)
			if backoff < 10*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = 500 * time.Millisecond
		for !ac.Closed() {
			time.Sleep(time.Second)
			if s.currentWatchSeq(spec.Name) != seq {
				return
			}
			if st, err := s.instance(spec).Running(); err != nil || st == nil {
				return
			}
		}
		s.dropAgent(spec.Name, ac)
		time.Sleep(backoff)
	}
}
