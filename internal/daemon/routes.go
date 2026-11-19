package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

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
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	if _, err := s.Clusters.Ensure(cluster.DefaultClusterID); err != nil {
		writeErr(w, 500, err)
		return
	}
	previous := s.Clusters.Get(cluster.DefaultClusterID)
	if err := s.Clusters.SetDefaultExit(cluster.DefaultClusterID, req.Via); err != nil {
		writeErr(w, 500, err)
		return
	}
	if err := s.pushGatewayConfig(cluster.DefaultClusterID); err != nil {
		writeErr(w, 500, rollbackRoute("default route", err, func() error {
			return s.Clusters.SetDefaultExit(cluster.DefaultClusterID, previous.DefaultExit)
		}, func() error { return s.pushGatewayConfig(cluster.DefaultClusterID) }))
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
	eng, err := rules.NewEngine("", nil)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if err := eng.Add(rule); err != nil {
		writeErr(w, 400, err)
		return
	}
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	if _, err := s.Clusters.Ensure(cluster.DefaultClusterID); err != nil {
		writeErr(w, 500, err)
		return
	}
	previous := s.Clusters.Get(cluster.DefaultClusterID)
	idx, err := s.Clusters.AddRule(cluster.DefaultClusterID, rule)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if err := s.pushGatewayConfig(cluster.DefaultClusterID); err != nil {
		writeErr(w, 500, rollbackRoute("rule", err, func() error {
			return s.Clusters.SetRules(cluster.DefaultClusterID, previous.Rules)
		}, func() error { return s.pushGatewayConfig(cluster.DefaultClusterID) }))
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
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	previous := s.Clusters.Get(cluster.DefaultClusterID)
	if err := s.Clusters.RemoveRule(cluster.DefaultClusterID, idx); err != nil {
		writeErr(w, 404, err)
		return
	}
	if err := s.pushGatewayConfig(cluster.DefaultClusterID); err != nil {
		writeErr(w, 500, rollbackRoute("rule removal", err, func() error {
			return s.Clusters.SetRules(cluster.DefaultClusterID, previous.Rules)
		}, func() error { return s.pushGatewayConfig(cluster.DefaultClusterID) }))
		return
	}
	writeJSON(w, 200, map[string]any{"removed": idx})
}

// The caller holds routeMu across the initial state change, gateway push,
// and compensation so a stale snapshot cannot erase another route update.
func rollbackRoute(label string, cause error, restore, push func() error) error {
	if err := restore(); err != nil {
		return errors.Join(fmt.Errorf("%s gateway push failed: %w", label, cause),
			fmt.Errorf("rollback save failed; inspect `pvm route ls` and retry the route command: %w", err))
	}
	if err := push(); err != nil {
		return errors.Join(fmt.Errorf("%s state rolled back after gateway push failed: %w", label, cause),
			fmt.Errorf("rollback config push failed; fix the gateway and retry the route command: %w", err))
	}
	return fmt.Errorf("%s rolled back after gateway push failed: %w", label, cause)
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
	s.cancelAgentInstall(name)
	defer s.resumeAgentInstallIfRunning(name)
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
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	previousExit := ""
	if cl := s.Clusters.Get(fresh.ClusterID); cl != nil {
		previousExit = cl.VMExits[fresh.Name]
	}
	ops := vmRouteOps{
		running: func() (bool, error) {
			st, err := s.instance(fresh).Running()
			return st != nil, err
		},
		stop:  func() error { return s.instance(fresh).Stop() },
		start: func() error { return s.startForRouteSwitch(fresh) },
		setExit: func(v string) error {
			return s.Clusters.SetVMExit(fresh.ClusterID, fresh.Name, v)
		},
		push: func() error { return s.pushGatewayConfig(fresh.ClusterID) },
		deferRegion: func() error {
			fresh.RegionPending = true
			if err := fresh.Save(s.P); err != nil {
				fresh.RegionPending = false
				return fmt.Errorf("save %s spec: %w", fresh.Name, err)
			}
			s.cacheSpec(fresh)
			return nil
		},
		gatewayRunning: func() (bool, error) {
			cl := s.Clusters.Get(fresh.ClusterID)
			if cl == nil {
				return false, nil
			}
			gw, err := s.getSpec(cl.Gateway)
			if errors.Is(err, errVMNotFound) {
				return false, nil
			}
			if err != nil {
				return false, err
			}
			st, err := s.instance(gw).Running()
			return st != nil, err
		},
	}
	res, err := s.switchVMRoute(fresh, via, previousExit, ops)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	s.mu.Lock()
	s.vms[name] = fresh
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"vm": spec.Name, "via": via, "note": res.note, "restarted": res.restarted, "pushed": res.pushed})
}

// vmRouteOps are the side-effecting steps of a VM route switch, injected
// so the lifecycle orchestration stays testable without QEMU or a live
// gateway.
type vmRouteOps struct {
	running func() (bool, error)
	stop    func() error
	start   func() error
	setExit func(via string) error
	push    func() error
	// gatewayRunning and deferRegion are used only for a stopped VM.
	gatewayRunning func() (bool, error)
	// deferRegion leaves the exit's timezone/locale to the next start.
	deferRegion func() error
}

type vmRouteResult struct {
	note      string
	restarted bool
	// pushed is false when the exit was only saved for the next start.
	pushed bool
}

// switchVMRoute stops a running VM before switching its exit and restarts
// it once the new config is pushed: the guest re-converges DHCP/NAT only
// across a reboot, so a live switch would leave it on the old exit. A
// failed push rolls the route back to previousExit, and every failure
// path names the next command. A failed rollback leaves the VM stopped.
//
// A stopped VM without a running gateway only has its exit saved: there
// is nothing to push to, and the VM's next start boots the gateway and
// pushes the config before the VM itself boots. That lets a new VM get
// its exit before its first boot.
//
// The exit's timezone/locale are set over ssh, so a stopped VM gets them
// at its next start, which fails visibly when they cannot be set.
func (s *Server) switchVMRoute(spec *vm.Spec, via, previousExit string, ops vmRouteOps) (vmRouteResult, error) {
	var none vmRouteResult
	wasRunning, err := ops.running()
	if err != nil {
		return none, err
	}
	toExit := via != "" && via != "direct"
	regionDeferred := !wasRunning && (toExit || spec.PrevTZ != "" || spec.PrevLocale != "")
	if wasRunning {
		if err := ops.stop(); err != nil {
			return none, fmt.Errorf("cannot stop %s to switch its route: %w (route unchanged)", spec.Name, err)
		}
	} else if regionDeferred {
		// Before the save: a rollback then only costs one more apply of
		// the previous exit's profile.
		if err := ops.deferRegion(); err != nil {
			return none, fmt.Errorf("vm route unchanged: %w", err)
		}
	}
	restore := func() error {
		if !wasRunning {
			return nil
		}
		return ops.start()
	}
	if err := ops.setExit(via); err != nil {
		if rerr := restore(); rerr != nil {
			return none, fmt.Errorf("%w (additionally failed to restart %s: %v; run `pvm start %s`)", err, spec.Name, rerr, spec.Name)
		}
		return none, err
	}
	push := ops.push
	pushed := true
	if !wasRunning {
		// asked after the save, so a gateway that starts meanwhile
		// renders the new exit
		up, err := ops.gatewayRunning()
		if err != nil {
			if rbErr := ops.setExit(previousExit); rbErr != nil {
				return none, errors.Join(err, fmt.Errorf("route rollback save failed; inspect `pvm route ls` and retry `pvm route %s --via %s`: %w", spec.Name, routeExitLabel(via), rbErr))
			}
			return none, fmt.Errorf("vm route unchanged, cannot tell whether the gateway runs: %w", err)
		}
		if !up {
			pushed = false
			push = func() error { return nil }
		}
	}
	if err := push(); err != nil {
		next := fmt.Sprintf("fix the gateway and retry `pvm route %s --via %s`", spec.Name, routeExitLabel(via))
		if rbErr := ops.setExit(previousExit); rbErr != nil {
			return none, errors.Join(fmt.Errorf("gateway push failed: %w", err),
				fmt.Errorf("route rollback save failed; VM remains stopped; %s: %w", next, rbErr))
		}
		if rbErr := push(); rbErr != nil {
			return none, errors.Join(fmt.Errorf("gateway push failed: %w", err),
				fmt.Errorf("route state rolled back to %s, but rollback config push failed; VM remains stopped; %s: %w", routeExitLabel(previousExit), next, rbErr))
		}
		rollbackErr := fmt.Errorf("vm route rolled back to %s after gateway push failed; %s: %w", routeExitLabel(previousExit), next, err)
		if rerr := restore(); rerr != nil {
			return none, errors.Join(rollbackErr, fmt.Errorf("restarting %s failed; run `pvm start %s`: %w", spec.Name, spec.Name, rerr))
		}
		return none, rollbackErr
	}
	res := vmRouteResult{pushed: pushed}
	if !wasRunning {
		if regionDeferred {
			res.note = fmt.Sprintf("timezone/locale are set for this route when %s next starts", spec.Name)
		}
		return res, nil
	}
	if err := ops.start(); err != nil {
		return none, fmt.Errorf("route switched to %s, but restarting %s failed: %w", routeExitLabel(via), spec.Name, err)
	}
	res.restarted = true
	if toExit {
		res.note = s.applyRegionProfile(spec, via)
	} else {
		res.note = s.restoreRegion(spec)
	}
	return res, nil
}

// routeSwitchReadyTimeout bounds the post-restart ssh wait so a route
// switch cannot hang the CLI longer than a plain `pvm start` follow.
const routeSwitchReadyTimeout = 3 * time.Minute

// startForRouteSwitch restarts a VM after a route switch and waits for
// ssh readiness. The route is already switched at this point, so every
// error names the exact recovery command instead of a bare failure.
func (s *Server) startForRouteSwitch(spec *vm.Spec) error {
	task := s.beginAgentInstall(spec.Name)
	defer s.finishAgentInstall(spec.Name, task)
	inst, man, err := s.startVM(task.ctx, spec)
	if err != nil {
		return fmt.Errorf("vm failed to start: %w; run `pvm start %s`", err, spec.Name)
	}
	s.ensureAgentAsync(spec)
	if man != nil {
		if err := inst.WaitRestored(s.P.VMDir(spec.Name)); err != nil {
			inst.Stop()
			return fmt.Errorf("snapshot restore failed, vm stopped: %w; run `pvm start %s`", err, spec.Name)
		}
	}
	if err := s.waitSSH(spec, routeSwitchReadyTimeout); err != nil {
		return fmt.Errorf("vm started but never became ssh-ready: %w; check `pvm logs %s`", err, spec.Name)
	}
	return nil
}

func routeExitLabel(via string) string {
	if via == "" {
		return "direct"
	}
	return via
}

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
	out, err := s.sshRun(gw, "sudo cat /run/pvm/fakeip.json")
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
