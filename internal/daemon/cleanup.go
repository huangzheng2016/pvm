package daemon

import "github.com/huangzheng2016/pvm/internal/cluster"

// removeVMMapsLocked removes the relays of every map the VM currently
// holds in the store, then drops the VM itself. It reads the store
// inside the critical section so a map committed after the caller took
// its cluster snapshot still gets its relay removed. Callers hold
// mapMu.
func (s *Server) removeVMMapsLocked(clusterID int, name string) {
	if cl := s.Clusters.Get(clusterID); cl != nil {
		for _, m := range cl.Maps {
			if m.VM == name {
				s.relays.Remove(m.Proto, m.HostPort)
			}
		}
	}
	s.Clusters.RemoveVM(clusterID, name)
}

// Close resources before the cluster forgets the VM so no port remains
// bound for a later VM to inherit. Callers hold mapMu.
func (s *Server) closeVMResources(name string, cl *cluster.Cluster) {
	if cl != nil {
		s.removeVMMapsLocked(cl.ID, name)
	}
	s.agentMu.Lock()
	if c := s.agents[name]; c != nil {
		c.Close()
		delete(s.agents, name)
	}
	s.agentMu.Unlock()
}
