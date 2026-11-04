package daemon

import (
	"context"

	"github.com/huangzheng2016/pvm/internal/cluster"
)

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

// removeVMMember drops a cluster membership under the gate with the
// rollback budget. On timeout it removes nothing and returns the lock
// error: the caller surfaces a retryable error, and the same create or
// clone completes the cleanup later (AddVM is idempotent for an
// existing member). No background work is spawned — an async remover
// could otherwise delete a later same-named VM's state.
func (s *Server) removeVMMember(clusterID int, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), rollbackLockTimeout)
	defer cancel()
	if err := s.mapMu.Lock(ctx); err != nil {
		return err
	}
	defer s.mapMu.Unlock()
	s.Clusters.RemoveVM(clusterID, name)
	return nil
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
