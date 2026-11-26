package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/huangzheng2016/pvm/internal/fsutil"
)

const (
	ProvisionPending = "pending"
	ProvisionRunning = "running"
	ProvisionReady   = "ready"
	ProvisionFailed  = "failed"
)

// VMStatus is persisted to vms/<name>/status.json so provisioning failures,
// readiness and desired run state survive daemon restarts and host reboots.
type VMStatus struct {
	ProvisionState string       `json:"provision_state"`
	AgentReady     bool         `json:"agent_ready"`
	DesiredRunning bool         `json:"desired_running"`
	LastError      string       `json:"last_error,omitempty"`
	CurrentJob     string       `json:"current_job,omitempty"`
	UpdatedAt      time.Time    `json:"updated_at"`
	Stages         []StageEvent `json:"stages,omitempty"`
}

// ClusterStatus models cluster/gateway readiness separately from QEMU process
// liveness and from the daemon's ability to serve API requests.
type ClusterStatus struct {
	ID                int        `json:"id"`
	Gateway           string     `json:"gateway"`
	GatewayRunning    bool       `json:"gateway_running"`
	GatewayAgentReady bool       `json:"gateway_agent_ready"`
	ConfigPushedAt    *time.Time `json:"config_pushed_at,omitempty"`
	Ready             bool       `json:"ready"`
	LastError         string     `json:"last_error,omitempty"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

func (s *Server) statusPath(name string) string {
	return filepath.Join(s.P.VMDir(name), "status.json")
}

// vmStatusLocked returns the cached status, lazily loading it from disk once.
// Caller must hold s.progMu.
func (s *Server) vmStatusLocked(name string) *VMStatus {
	if s.vmStatus == nil {
		s.vmStatus = map[string]*VMStatus{}
	}
	if st, ok := s.vmStatus[name]; ok {
		return st
	}
	st := &VMStatus{ProvisionState: ProvisionPending}
	if s.P == nil {
		s.vmStatus[name] = st
		return st
	}
	if raw, err := os.ReadFile(s.statusPath(name)); err == nil {
		var loaded VMStatus
		if json.Unmarshal(raw, &loaded) == nil {
			if loaded.ProvisionState == "" {
				loaded.ProvisionState = ProvisionPending
			}
			st = &loaded
		}
	}
	// A previously active provisioning run cannot still be live after a daemon restart.
	if st.ProvisionState == ProvisionRunning {
		st.ProvisionState = ProvisionPending
		st.AgentReady = false
	}
	s.vmStatus[name] = st
	return st
}

func (s *Server) getVMStatus(name string) VMStatus {
	s.progMu.Lock()
	defer s.progMu.Unlock()
	st := *s.vmStatusLocked(name)
	st.Stages = append([]StageEvent(nil), st.Stages...)
	return st
}

// persistVMStatusLocked writes status.json with AtomicWrite, throttling
// intermediate progress updates while always flushing state transitions.
// Caller must hold s.progMu.
func (s *Server) persistVMStatusLocked(name string, force bool) {
	if s.P == nil {
		return
	}
	st := s.vmStatusLocked(name)
	st.UpdatedAt = time.Now()
	st.Stages = append([]StageEvent(nil), s.prog[name]...)
	if len(st.Stages) > 16 {
		st.Stages = st.Stages[len(st.Stages)-16:]
	}
	if s.statusSaved == nil {
		s.statusSaved = map[string]time.Time{}
	}
	if !force && time.Since(s.statusSaved[name]) < 2*time.Second {
		return
	}
	if _, err := os.Stat(filepath.Dir(s.statusPath(name))); err != nil {
		return
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	if err := fsutil.AtomicWrite(s.statusPath(name), data, 0o600); err == nil {
		s.statusSaved[name] = time.Now()
	}
}

func (s *Server) setDesiredRunning(name string, desired bool) {
	s.progMu.Lock()
	defer s.progMu.Unlock()
	st := s.vmStatusLocked(name)
	if st.DesiredRunning != desired {
		st.DesiredRunning = desired
		s.persistVMStatusLocked(name, true)
	}
}

func (s *Server) updateClusterStatus(id int, update func(*ClusterStatus)) {
	s.clusterMu.Lock()
	defer s.clusterMu.Unlock()
	if s.clusterStatus == nil {
		s.clusterStatus = map[int]*ClusterStatus{}
	}
	cs := s.clusterStatus[id]
	if cs == nil {
		cs = &ClusterStatus{ID: id}
		if s.Clusters != nil {
			if cl := s.Clusters.Get(id); cl != nil {
				cs.Gateway = cl.Gateway
			}
		}
		s.clusterStatus[id] = cs
	}
	update(cs)
	cs.Ready = cs.GatewayRunning && cs.GatewayAgentReady && cs.LastError == ""
	cs.UpdatedAt = time.Now()
}

func (s *Server) markClusterError(id int, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	s.updateClusterStatus(id, func(cs *ClusterStatus) { cs.LastError = msg })
}

func (s *Server) clusterStatuses() []ClusterStatus {
	s.clusterMu.Lock()
	defer s.clusterMu.Unlock()
	var out []ClusterStatus
	if s.Clusters != nil {
		for _, cl := range s.Clusters.List() {
			cs := ClusterStatus{ID: cl.ID, Gateway: cl.Gateway}
			if cur := s.clusterStatus[cl.ID]; cur != nil {
				cs = *cur
			}
			out = append(out, cs)
		}
	}
	return out
}

func isNotFoundErr(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, errVMNotFound)
}
