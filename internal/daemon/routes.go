package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/huangzheng2016/pvm/internal/cluster"
	"github.com/huangzheng2016/pvm/internal/gw/rules"
)

type routeRuleRequest struct {
	Type    string `json:"type"`    // domain-suffix | domain-prefix | domain-keyword | cidr
	Pattern string `json:"pattern"` // e.g. example.com / 10.0.0.0/8
	Action  string `json:"action"`  // direct | proxy
	Via     string `json:"via"`     // exit-node name when action == proxy
}

func (s *Server) handleRouteList(w http.ResponseWriter, r *http.Request) {
	cl := s.Clusters.Get(cluster.DefaultClusterID)
	out := map[string]any{"rules": []rules.Rule{}, "vm_exits": map[string]string{}}
	if cl != nil {
		if cl.Rules != nil {
			out["rules"] = cl.Rules
		}
		if cl.VMExits != nil {
			out["vm_exits"] = cl.VMExits
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleRouteAdd(w http.ResponseWriter, r *http.Request) {
	var req routeRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	rule := rules.Rule{Type: rules.Type(req.Type), Pattern: req.Pattern, Action: rules.Action(req.Action), Exit: req.Via}
	if rule.Action == rules.Direct {
		rule.Exit = ""
	}
	// validate by compiling against a scratch engine
	eng, err := rules.NewEngine("", nil)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if err := eng.Add(rule); err != nil {
		writeErr(w, 400, err)
		return
	}
	if _, err := s.Clusters.Ensure(cluster.DefaultClusterID); err != nil {
		writeErr(w, 500, err)
		return
	}
	idx, err := s.Clusters.AddRule(cluster.DefaultClusterID, rule)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if cl := s.Clusters.Get(cluster.DefaultClusterID); cl != nil {
		if err := s.pushGatewayConfig(cl); err != nil {
			writeErr(w, 500, fmt.Errorf("rule stored but gateway push failed: %w", err))
			return
		}
	}
	writeJSON(w, 201, map[string]any{"index": idx, "rule": rule})
}

func (s *Server) handleRouteRemove(w http.ResponseWriter, r *http.Request) {
	idx, err := strconv.Atoi(r.PathValue("index"))
	if err != nil {
		writeErr(w, 400, fmt.Errorf("bad index"))
		return
	}
	if err := s.Clusters.RemoveRule(cluster.DefaultClusterID, idx); err != nil {
		writeErr(w, 404, err)
		return
	}
	if cl := s.Clusters.Get(cluster.DefaultClusterID); cl != nil {
		if err := s.pushGatewayConfig(cl); err != nil {
			writeErr(w, 500, fmt.Errorf("rule removed but gateway push failed: %w", err))
			return
		}
	}
	writeJSON(w, 200, map[string]any{"removed": idx})
}

type vmRouteRequest struct {
	Via string `json:"via"` // exit-node name, or "direct"
}

func (s *Server) handleVMRoute(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	spec, err := s.getSpec(name)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	if spec.Role == "gateway" || spec.ClusterID == 0 {
		writeErr(w, 400, fmt.Errorf("vm %s is not a cluster vm", name))
		return
	}
	var req vmRouteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.Clusters.SetVMExit(spec.ClusterID, spec.Name, req.Via); err != nil {
		writeErr(w, 500, err)
		return
	}
	if cl := s.Clusters.Get(spec.ClusterID); cl != nil {
		if err := s.pushGatewayConfig(cl); err != nil {
			writeErr(w, 500, fmt.Errorf("route stored but gateway push failed: %w", err))
			return
		}
	}
	writeJSON(w, 200, map[string]any{"vm": spec.Name, "via": req.Via})
}

// handleDNSList returns the gateway's live fake-ip mapping table.
func (s *Server) handleDNSList(w http.ResponseWriter, r *http.Request) {
	cl := s.Clusters.Get(cluster.DefaultClusterID)
	if cl == nil {
		writeErr(w, 404, fmt.Errorf("no cluster yet"))
		return
	}
	gw, err := s.getSpec(cl.Gateway)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	out, err := s.sshRun(gw, "cat /run/pvm/fakeip.json 2>/dev/null || echo {}")
	if err != nil {
		writeErr(w, 500, fmt.Errorf("read gateway fake-ip table: %v\n%s", err, out))
		return
	}
	var table map[string]string
	if err := json.Unmarshal([]byte(out), &table); err != nil {
		writeErr(w, 500, fmt.Errorf("parse fake-ip table: %v", err))
		return
	}
	writeJSON(w, 200, table)
}
