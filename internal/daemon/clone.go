package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/cluster"
	"github.com/huangzheng2016/pvm/internal/qemu"
	"github.com/huangzheng2016/pvm/internal/vm"
)

type cloneMapRequest struct {
	HostPort  int    `json:"host_port"`
	GuestPort int    `json:"guest_port"`
	Proto     string `json:"proto"`
}

type cloneRequest struct {
	NewName string            `json:"new_name"`
	Start   bool              `json:"start"`
	Maps    []cloneMapRequest `json:"maps"`
}

func (s *Server) handleClone(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req cloneRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	src, err := s.getSpec(name)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	if src.Role == "gateway" {
		writeErr(w, 400, fmt.Errorf("gateway vms cannot be cloned"))
		return
	}
	if err := vm.ValidateName(req.NewName); err != nil {
		writeErr(w, 400, err)
		return
	}
	if _, err := vm.Load(s.P, req.NewName); err == nil {
		writeErr(w, 409, fmt.Errorf("vm %q already exists", req.NewName))
		return
	}
	if st := s.instance(src).Running(); st != nil {
		writeErr(w, 409, fmt.Errorf("source vm %s is running; stop it first — copying a live disk is unsafe", name))
		return
	}
	if src.ClusterID == 0 {
		writeErr(w, 400, fmt.Errorf("source vm %s is not a cluster vm", name))
		return
	}

	// Lock both names in a stable order to avoid deadlocks.
	a, b := name, req.NewName
	if b < a {
		a, b = b, a
	}
	la := qemu.LockVM(a)
	la.Lock()
	defer la.Unlock()
	lb := qemu.LockVM(b)
	lb.Lock()
	defer lb.Unlock()

	cl := s.Clusters.Get(src.ClusterID)
	if cl == nil {
		writeErr(w, 500, fmt.Errorf("cluster %d for %s not found", src.ClusterID, name))
		return
	}
	vnet, err := s.Clusters.AddVM(cl.ID, req.NewName)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	cleanup := func() {
		s.Clusters.RemoveVM(cl.ID, req.NewName)
		_ = vm.Delete(s.P, req.NewName)
	}

	spec := vm.CloneSpec(src, req.NewName, vnet.IP, vnet.MAC)
	if err := vm.CloneDisk(s.P, s.TC, src, spec); err != nil {
		cleanup()
		writeErr(w, 500, err)
		return
	}
	if err := spec.Save(s.P); err != nil {
		cleanup()
		writeErr(w, 500, err)
		return
	}
	s.mu.Lock()
	s.vms[spec.Name] = spec
	s.mu.Unlock()

	var added []*cluster.PortMap
	for _, m := range req.Maps {
		proto := m.Proto
		if proto == "" {
			proto = "tcp"
		}
		if m.GuestPort <= 0 || m.GuestPort > 65535 {
			writeErr(w, 400, fmt.Errorf("guest_port %d out of range", m.GuestPort))
			return
		}
		if m.HostPort < 1024 || m.HostPort > 65535 {
			writeErr(w, 400, fmt.Errorf("host_port %d must be 1024-65535", m.HostPort))
			return
		}
		if err := portFree(proto, m.HostPort); err != nil {
			writeErr(w, 409, fmt.Errorf("map %d:%d/%s: %w", m.HostPort, m.GuestPort, proto, err))
			return
		}
		pm, err := s.Clusters.AddMap(cl.ID, spec.Name, spec.IP, m.HostPort, m.GuestPort, proto)
		if err != nil {
			writeErr(w, 409, err)
			return
		}
		if err := s.relays.Ensure(pm.Proto, pm.HostPort, pm.PoolPort); err != nil {
			s.Clusters.RemoveMap(cl.ID, pm.HostPort, pm.Proto)
			writeErr(w, 500, err)
			return
		}
		added = append(added, pm)
	}
	if len(added) > 0 {
		if err := s.pushGatewayConfig(cl); err != nil {
			writeErr(w, 500, fmt.Errorf("clone created but gateway config push failed: %w", err))
			return
		}
	}

	if req.Start {
		if _, _, err := s.startVM(spec); err != nil {
			writeErr(w, 500, fmt.Errorf("cloned but start failed: %w", err))
			return
		}
		s.ensureAgentAsync(spec)
		s.maybeIdentityReset(spec)
	}
	writeJSON(w, 201, map[string]any{"spec": spec, "maps": added, "running": req.Start})
}

// maybeIdentityReset regenerates guest identity (hostname, machine-id,
// ssh host keys) and rewrites netplan (the source's cloud-init pinned
// DHCP to the source MAC, so a clone has no network until netplan is
// fixed). The agent channel is preferred — it needs no guest network;
// ssh is the fallback. Runs in the background; failures are logged,
// never fatal.
func (s *Server) maybeIdentityReset(spec *vm.Spec) {
	if !spec.IdentityReset {
		return
	}
	go func() {
		if err := s.identityReset(spec); err != nil {
			log.Printf("clone %s: identity reset failed: %v", spec.Name, err)
			return
		}
		spec.IdentityReset = false
		if err := spec.Save(s.P); err != nil {
			log.Printf("clone %s: identity reset flag save: %v", spec.Name, err)
		}
		log.Printf("clone %s: guest identity regenerated", spec.Name)
	}()
}

func (s *Server) identityReset(spec *vm.Spec) error {
	script := identityResetScript(spec.Name)

	// Agent channel first (no network dependency) via the shared pool:
	// QEMU's chardev accepts exactly one host connection, so dialing
	// around the pool starves every other agent user.
	deadline := time.Now().Add(3 * time.Minute)
	var lastErr error
	for time.Now().Before(deadline) {
		ac, err := s.agentClient(spec.Name)
		if err != nil {
			lastErr = err
			time.Sleep(3 * time.Second)
			continue
		}
		var stderrBuf bytes.Buffer
		res, err := ac.Exec([]string{"sh", "-c", script}, nil, nil, &stderrBuf, 120)
		if err != nil {
			s.dropAgent(spec.Name, ac)
			lastErr = err
			time.Sleep(2 * time.Second)
			continue
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("identity reset via agent: exit %d: %s", res.ExitCode, strings.TrimSpace(stderrBuf.String()))
		}
		return nil
	}

	// No agent in the guest: fall back to ssh (needs working network).
	if err := s.waitSSH(spec, 6*time.Minute); err != nil {
		return fmt.Errorf("identity reset unreachable (agent: %v): %w", lastErr, err)
	}
	if out, err := s.sshRun(spec, script); err != nil {
		return fmt.Errorf("identity reset via ssh: %v\n%s", err, out)
	}
	return nil
}

// identityResetScript is run once inside a fresh clone. ssh restart does
// not drop established sessions on Ubuntu, so our own connection lives.
// The netplan rewrite matches the primary NIC by name (en*) instead of
// the source's MAC, making future clones boot network-ready too.
func identityResetScript(newHostname string) string {
	return fmt.Sprintf(
		"printf 'network:\n  version: 2\n  ethernets:\n    primary:\n      match:\n        name: en*\n      dhcp4: true\n' | sudo tee /etc/netplan/50-cloud-init.yaml >/dev/null && "+
			"sudo netplan apply && "+
			"sudo hostnamectl set-hostname %[1]s && "+
			"sudo sh -c 'rm -f /etc/machine-id /var/lib/dbus/machine-id && systemd-machine-id-setup' && "+
			"sudo rm -f /etc/ssh/ssh_host_* && sudo ssh-keygen -A; "+
			"sudo systemctl restart ssh.socket ssh.service 2>/dev/null || true",
		newHostname)
}
