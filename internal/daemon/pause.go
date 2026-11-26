package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/huangzheng2016/pvm/internal/cluster"
	"github.com/huangzheng2016/pvm/internal/vm"
)

func (s *Server) handleClusterPush(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		writeErr(w, 400, fmt.Errorf("invalid cluster id"))
		return
	}
	if err := s.pushGatewayConfig(id); err != nil {
		writeErr(w, 502, err)
		return
	}
	writeJSON(w, 200, map[string]any{"pushed": id})
}

func (s *Server) handleRoutePause(resume bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			VM string `json:"vm"`
		}
		if r.Body != nil && r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeErr(w, 400, err)
				return
			}
		}
		if req.VM != "" {
			if err := vm.ValidateName(req.VM); err != nil {
				writeErr(w, 400, err)
				return
			}
			if spec, err := s.getSpec(req.VM); err != nil {
				writeErr(w, 404, err)
				return
			} else if spec.Role == "gateway" {
				writeErr(w, 400, fmt.Errorf("cannot pause the gateway VM"))
				return
			}
		}
		s.routeMu.Lock()
		defer s.routeMu.Unlock()
		id := cluster.DefaultClusterID
		if _, err := s.Clusters.Ensure(id); err != nil {
			writeErr(w, 500, err)
			return
		}
		paused := !resume
		if err := s.Clusters.SetPaused(id, req.VM, paused); err != nil {
			writeErr(w, 400, err)
			return
		}
		if err := s.pushGatewayConfig(id); err != nil {
			writeErr(w, 500, rollbackRoute("pause route", err, func() error {
				return s.Clusters.SetPaused(id, req.VM, !paused)
			}, func() error { return s.pushGatewayConfig(id) }))
			return
		}
		target := req.VM
		if target == "" {
			target = "*"
		}
		writeJSON(w, 200, map[string]any{"target": target, "paused": paused})
	}
}
