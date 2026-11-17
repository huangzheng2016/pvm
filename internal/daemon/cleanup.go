package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/huangzheng2016/pvm/internal/cluster"
	"github.com/huangzheng2016/pvm/internal/qemu"
	"github.com/huangzheng2016/pvm/internal/vm"
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

// errVMNotFound marks a VM whose vm.json does not exist.
var errVMNotFound = errors.New("not found")

// orphanMemberCluster returns the cluster that still lists name as a
// member although its VM files are gone, for example after ~/.pvm/vms
// was deleted by hand.
func (s *Server) orphanMemberCluster(name string) *cluster.Cluster {
	for _, cl := range s.Clusters.List() {
		if cl.Gateway != name && cl.VMs[name] != nil {
			return cl
		}
	}
	return nil
}

// deleteOrphanMember drops the cluster state of a member that has no
// vm.json. Without a spec pvm cannot stop it, so a run state that shows a
// live QEMU refuses instead of freeing an address that VM still uses.
func (s *Server) deleteOrphanMember(w http.ResponseWriter, name string, cl *cluster.Cluster) {
	s.cancelAgentInstall(name)
	lock := qemu.LockVM(name)
	lock.Lock()
	defer lock.Unlock()
	if _, err := vm.Load(s.P, name); !errors.Is(err, os.ErrNotExist) {
		writeErr(w, 409, fmt.Errorf("vm %q changed during delete; retry", name))
		return
	}
	in := &qemu.Instance{StatePath: filepath.Join(s.P.VMDir(name), "run.json")}
	if st, err := in.Running(); err != nil || st != nil {
		writeErr(w, 409, fmt.Errorf("vm %q has no vm.json but its QEMU may still be running (%s); stop that process, then retry", name, in.StatePath))
		return
	}
	s.resetStages(name)
	lockCtx, lockCancel := mapLockCtx()
	defer lockCancel()
	if err := s.mapMu.Lock(lockCtx); err != nil {
		writeErr(w, 500, fmt.Errorf("map lock: %v; retry the delete", err))
		return
	}
	defer s.mapMu.Unlock()
	if err := s.closeVMResources(name, cl); err != nil {
		writeErr(w, 500, fmt.Errorf("remove cluster state: %w; retry delete", err))
		return
	}
	cleanupErr := errors.Join(s.removeLink(cl, name), s.pushBounded(cl.ID))
	if err := vm.Delete(s.P, name); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.mu.Lock()
	delete(s.vms, name)
	s.mu.Unlock()
	if cleanupErr != nil {
		writeErr(w, 500, fmt.Errorf("leftover membership deleted, but gateway cleanup is incomplete: %w; run `pvm daemon restart` to reconcile gateway state", cleanupErr))
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": name, "orphan": true})
}
