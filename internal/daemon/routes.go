package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/huangzheng2016/pvm/internal/cluster"
	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/gw/rules"
	"github.com/huangzheng2016/pvm/internal/qemu"
	"github.com/huangzheng2016/pvm/internal/vm"
)

type routeRuleRequest struct {
	Type    string `json:"type"`    // domain-suffix | domain-prefix | domain-keyword | cidr
	Pattern string `json:"pattern"` // e.g. example.com / 10.0.0.0/8
	Action  string `json:"action"`  // direct | proxy | internal
	Via     string `json:"via"`     // exit-node name when action == proxy
	DNS     string `json:"dns"`     // designated resolver host[:port] when action == internal
}

func (s *Server) handleRouteList(w http.ResponseWriter, r *http.Request) {
	cl := s.Clusters.Get(cluster.DefaultClusterID)
	out := map[string]any{"rules": []rules.Rule{}, "vm_exits": map[string]string{}, "default_exit": ""}
	if cl != nil {
		if cl.Rules != nil {
			out["rules"] = cl.Rules
		}
		if cl.VMExits != nil {
			out["vm_exits"] = cl.VMExits
		}
		out["default_exit"] = cl.DefaultExit
	}
	writeJSON(w, 200, out)
}

// handleDefaultRoute sets the cluster-wide fallback route: an exit name,
// or "direct"/"" to clear it.
func (s *Server) handleDefaultRoute(w http.ResponseWriter, r *http.Request) {
	var req vmRouteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if req.Via != "" && req.Via != "direct" {
		if _, err := exit.LoadEntry(filepath.Join(s.P.Home, "exits"), req.Via); err != nil {
			writeErr(w, 400, fmt.Errorf("exit %q is not registered (see `pvm exit ls`)", req.Via))
			return
		}
	}
	if _, err := s.Clusters.Ensure(cluster.DefaultClusterID); err != nil {
		writeErr(w, 500, err)
		return
	}
	if err := s.Clusters.SetDefaultExit(cluster.DefaultClusterID, req.Via); err != nil {
		writeErr(w, 500, err)
		return
	}
	if err := s.pushGatewayConfig(cluster.DefaultClusterID); err != nil {
		writeErr(w, 500, fmt.Errorf("route stored but gateway push failed: %w", err))
		return
	}
	via := req.Via
	if via == "" {
		via = "direct"
	}
	writeJSON(w, 200, map[string]any{"default_exit": via})
}

func (s *Server) handleRouteAdd(w http.ResponseWriter, r *http.Request) {
	var req routeRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	rule := rules.Rule{Type: rules.Type(req.Type), Pattern: req.Pattern, Action: rules.Action(req.Action), Exit: req.Via, DNS: req.DNS}
	switch rule.Action {
	case rules.Direct:
		rule.Exit, rule.DNS = "", ""
	case rules.Proxy:
		rule.DNS = ""
		if _, err := exit.LoadEntry(filepath.Join(s.P.Home, "exits"), rule.Exit); err != nil {
			writeErr(w, 400, fmt.Errorf("exit %q is not registered (see `pvm exit ls`)", rule.Exit))
			return
		}
	case rules.Internal:
		rule.Exit = ""
		if rule.Type != rules.DomainSuffix && rule.Type != rules.DomainPrefix && rule.Type != rules.DomainKeyword {
			writeErr(w, 400, fmt.Errorf("internal rules need a domain match type"))
			return
		}
	default:
		writeErr(w, 400, fmt.Errorf("unknown action %q", rule.Action))
		return
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
	if err := s.pushGatewayConfig(cluster.DefaultClusterID); err != nil {
		writeErr(w, 500, fmt.Errorf("rule stored but gateway push failed: %w", err))
		return
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
	if err := s.pushGatewayConfig(cluster.DefaultClusterID); err != nil {
		writeErr(w, 500, fmt.Errorf("rule removed but gateway push failed: %w", err))
		return
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
	via := req.Via
	if via != "" && via != "direct" {
		if _, err := exit.LoadEntry(filepath.Join(s.P.Home, "exits"), via); err != nil {
			writeErr(w, 400, fmt.Errorf("exit %q is not registered (see `pvm exit ls`)", via))
			return
		}
	}
	lock := qemu.LockVM(name)
	lock.Lock()
	defer lock.Unlock()
	// The region profile mutates PrevTZ/PrevLocale on a fresh copy; the
	// cached spec is only replaced afterwards, so concurrent readers
	// never observe a half-updated struct.
	fresh, err := vm.Load(s.P, name)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if err := s.Clusters.SetVMExit(spec.ClusterID, spec.Name, via); err != nil {
		writeErr(w, 500, err)
		return
	}
	if err := s.pushGatewayConfig(spec.ClusterID); err != nil {
		writeErr(w, 500, fmt.Errorf("route stored but gateway push failed: %w", err))
		return
	}
	note := ""
	if via == "direct" || via == "" {
		note = s.restoreRegion(fresh)
	} else {
		note = s.applyRegionProfile(fresh, via)
	}
	s.mu.Lock()
	s.vms[name] = fresh
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"vm": spec.Name, "via": via, "note": note})
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
