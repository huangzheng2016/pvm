package daemon

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/huangzheng2016/pvm/internal/cluster"
	"github.com/huangzheng2016/pvm/internal/fsutil"
	"github.com/huangzheng2016/pvm/internal/netcheck"
)

func (s *Server) handleRequireCheck(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	off := r.URL.Query().Get("off") == "1"
	if _, err := s.getSpec(name); err != nil {
		writeErr(w, 404, err)
		return
	}
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	id := cluster.DefaultClusterID
	if err := s.Clusters.SetRequireCheck(id, name, !off); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.pushGatewayConfig(id); err != nil {
		writeErr(w, 500, rollbackRoute("require-check", err, func() error {
			return s.Clusters.SetRequireCheck(id, name, off)
		}, func() error { return s.pushGatewayConfig(id) }))
		return
	}
	if !off {
		// Issue the first lease immediately if the VM passes a fresh check.
		go s.renewVMCheckLease(name, true)
	}
	writeJSON(w, 200, map[string]any{"vm": name, "require_check": !off})
}

func (s *Server) renewVMCheckLease(name string, forceCheck bool) {
	spec, err := s.getSpec(name)
	if err != nil || spec.Role == "gateway" || !s.vmReady(name) {
		return
	}
	cl := s.Clusters.Get(spec.ClusterID)
	if cl == nil || !cl.RequireCheck[name] {
		return
	}
	gw, err := s.getSpec(cl.Gateway)
	if err != nil {
		return
	}
	var rep netcheck.Report
	needRun := forceCheck
	if raw, err := os.ReadFile(s.checkPath(name)); err == nil && json.Unmarshal(raw, &rep) == nil {
		if time.Since(rep.CreatedAt) > 5*time.Minute {
			needRun = true
		}
	} else {
		needRun = true
	}
	if needRun {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		fresh, err := s.runVMCheck(ctx, spec)
		cancel()
		if err != nil {
			log.Printf("admission check %s: %v", name, err)
			return
		}
		rep = *fresh
		if data, err := json.MarshalIndent(&rep, "", "  "); err == nil {
			_ = fsutil.AtomicWrite(s.checkPath(name), data, 0o600)
		}
	}
	if !rep.Authorizes("", "") {
		log.Printf("admission %s: check not authorized; lease will expire", name)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := s.sshRunCtx(ctx, gw, s.gatewayBinCmd("__grant", "--vm", name, "--seconds", "90")); err != nil {
		log.Printf("admission grant %s: %v", name, err)
	}
}

func (s *Server) runAdmissionLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for _, cl := range s.Clusters.List() {
				for vmName := range cl.RequireCheck {
					s.renewVMCheckLease(vmName, false)
				}
			}
		}
	}
}
