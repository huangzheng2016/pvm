package daemon

import (
	"context"
	"log"
	"time"

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

// removeVMWithRetry drops a cluster membership under the gate. On lock
// timeout it keeps retrying in the background (bounded) so a failed
// create never silently leaves an orphan member.
func (s *Server) removeVMWithRetry(clusterID int, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), rollbackLockTimeout)
	err := s.mapMu.Lock(ctx)
	cancel()
	if err == nil {
		s.Clusters.RemoveVM(clusterID, name)
		s.mapMu.Unlock()
		return
	}
	log.Printf("cluster %d: remove %s: map lock: %v; retrying in background", clusterID, name, err)
	go func() {
		for i := 0; i < 60; i++ {
			time.Sleep(5 * time.Second)
			ctx, cancel := context.WithTimeout(context.Background(), rollbackLockTimeout)
			if err := s.mapMu.Lock(ctx); err == nil {
				s.Clusters.RemoveVM(clusterID, name)
				s.mapMu.Unlock()
				cancel()
				log.Printf("cluster %d: removed %s after retry", clusterID, name)
				return
			} else {
				cancel()
			}
		}
		log.Printf("cluster %d: giving up removing %s after retries; remove it manually", clusterID, name)
	}()
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
