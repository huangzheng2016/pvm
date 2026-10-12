package daemon

import "github.com/huangzheng2016/pvm/internal/cluster"

// Close resources before the cluster forgets the VM so no port remains
// bound for a later VM to inherit.
func (s *Server) closeVMResources(name string, cl *cluster.Cluster) {
	if cl != nil {
		for _, m := range cl.Maps {
			if m.VM == name {
				s.relays.Remove(m.Proto, m.HostPort)
			}
		}
	}
	s.agentMu.Lock()
	if c := s.agents[name]; c != nil {
		c.Close()
		delete(s.agents, name)
	}
	s.agentMu.Unlock()
}
