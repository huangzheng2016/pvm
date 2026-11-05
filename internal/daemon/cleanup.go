package daemon

import (
	"context"

	"github.com/huangzheng2016/pvm/internal/cluster"
)

// removeVMMapsLocked commits membership removal before releasing relays.
// A failed state write leaves both the VM and its live mappings intact.
// Callers hold mapMu.
func (s *Server) removeVMMapsLocked(clusterID int, name string) error {
	cl := s.Clusters.Get(clusterID)
	if err := s.Clusters.RemoveVM(clusterID, name); err != nil {
		return err
	}
	if cl != nil {
		for _, m := range cl.Maps {
			if m.VM == name {
				s.relays.Remove(m.Proto, m.HostPort)
			}
		}
	}
	return nil
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
	return s.Clusters.RemoveVM(clusterID, name)
}

// Close resources before the cluster forgets the VM so no port remains
// bound for a later VM to inherit. Callers hold mapMu.
func (s *Server) closeVMResources(name string, cl *cluster.Cluster) error {
	if cl != nil {
		if err := s.removeVMMapsLocked(cl.ID, name); err != nil {
			return err
		}
	}
	s.invalidateAgent(name)
	return nil
}
