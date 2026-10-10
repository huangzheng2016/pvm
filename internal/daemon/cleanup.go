package daemon

import "github.com/huangzheng2016/pvm/internal/cluster"

// closeVMResources tears down in-memory resources bound to a VM: host
// relays for its maps, the kasm loopback proxy and the shared agent
// connection. Called before the cluster forgets the VM, so nothing is
// left holding a port that a later VM could silently inherit.
func (s *Server) closeVMResources(name string, cl *cluster.Cluster) {
	if cl != nil {
		for _, m := range cl.Maps {
			if m.VM == name {
				s.relays.Remove(m.Proto, m.HostPort)
			}
		}
	}
	s.kasmMu.Lock()
	if kp := s.kasms[name]; kp != nil {
		kp.close()
		delete(s.kasms, name)
	}
	s.kasmMu.Unlock()
	s.agentMu.Lock()
	if c := s.agents[name]; c != nil {
		c.Close()
		delete(s.agents, name)
	}
	s.agentMu.Unlock()
}
