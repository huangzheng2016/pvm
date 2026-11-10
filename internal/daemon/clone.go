package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/cluster"
	"github.com/huangzheng2016/pvm/internal/keys"
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
	if req.NewName == name {
		writeErr(w, 409, fmt.Errorf("vm %q already exists", req.NewName))
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

	// The destination may have been created while this request waited.
	// Any existing directory, including one with an invalid spec, belongs
	// to its creator and must never enter clone rollback.
	if _, err := os.Lstat(s.P.VMDir(req.NewName)); err == nil {
		writeErr(w, 409, fmt.Errorf("vm directory %q already exists", req.NewName))
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		writeErr(w, 500, fmt.Errorf("inspect clone destination: %w", err))
		return
	}
	src, err = vm.Load(s.P, name)
	if err != nil {
		writeErr(w, 404, err)
		return
	}

	if st, err := s.instance(src).Running(); err != nil {
		writeErr(w, 500, err)
		return
	} else if st != nil {
		writeErr(w, 409, fmt.Errorf("source vm %s is running; stop it first — copying a live disk is unsafe", name))
		return
	}

	cl, vnet, err := s.prepareCloneNetwork(src, req.NewName)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	spec := vm.CloneSpec(src, req.NewName, vnet.IP, vnet.MAC)
	created := false
	cleanup := func() error {
		if err := s.removeVMMember(cl.ID, req.NewName); err != nil {
			if created {
				if saveErr := spec.Save(s.P); saveErr != nil {
					return fmt.Errorf("membership cleanup incomplete (%v), recovery spec save failed (%v); repair state storage before removing the partial clone", err, saveErr)
				}
				s.cacheSpec(spec)
				return fmt.Errorf("membership cleanup incomplete (%v); clone files retained; run `pvm rm %s` after repairing state storage", err, spec.Name)
			}
			return fmt.Errorf("membership cleanup incomplete (%v); no clone files were created; repair state storage and retry the clone", err)
		}
		if created {
			if err := vm.Delete(s.P, req.NewName); err != nil {
				return fmt.Errorf("clone file cleanup incomplete: %w", err)
			}
		}
		return nil
	}

	created, err = vm.CloneDisk(s.P, s.TC, src, spec)
	if err != nil {
		if cerr := cleanup(); cerr != nil {
			writeErr(w, 500, fmt.Errorf("clone failed (%v) and %v", err, cerr))
			return
		}
		writeErr(w, 500, err)
		return
	}
	if err := spec.Save(s.P); err != nil {
		if cerr := cleanup(); cerr != nil {
			writeErr(w, 500, fmt.Errorf("clone failed (%v) and %v", err, cerr))
			return
		}
		writeErr(w, 500, err)
		return
	}
	s.resetStages(spec.Name)
	s.cacheSpec(spec)

	var added []*cluster.PortMap
	// Inherit-ports commits under mapMu through the same validation as
	// every other map creation (membership, global port ownership),
	// without spanning the (slow) VM start below.
	ctx, cancel := mapLockCtx()
	added, code, err := s.cloneInheritMaps(ctx, spec, req.Maps)
	cancel()
	if err != nil {
		if rerr := s.rollbackCloneVM(cl, spec); rerr != nil {
			writeErr(w, 500, fmt.Errorf("clone failed (%v) and %v", err, rerr))
			return
		}
		writeErr(w, code, err)
		return
	}

	if req.Start {
		task := s.beginAgentInstall(spec.Name)
		_, _, err := s.startVM(task.ctx, spec)
		s.finishAgentInstall(spec.Name, task)
		if err != nil {
			if rerr := s.rollbackCloneVM(cl, spec); rerr != nil {
				writeErr(w, 500, fmt.Errorf("cloned but start failed (%v) and %v", err, rerr))
				return
			}
			writeErr(w, 500, fmt.Errorf("cloned but start failed: %w", err))
			return
		}
		s.maybeIdentityReset(spec)
	}
	writeJSON(w, 201, map[string]any{"spec": spec, "maps": added, "running": req.Start})
}

// prepareCloneNetwork pins the source's effective default before any clone
// process can run. Explicit direct remains direct if the cluster default
// changes later; global destination rules still apply to both VMs.
func (s *Server) prepareCloneNetwork(src *vm.Spec, name string) (*cluster.Cluster, *cluster.VMNet, error) {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	cl := s.Clusters.Get(src.ClusterID)
	if cl == nil {
		return nil, nil, fmt.Errorf("cluster %d for %s not found", src.ClusterID, src.Name)
	}
	if _, exists := cl.VMs[name]; exists {
		return nil, nil, fmt.Errorf("cluster already contains %q; existing membership retained, inspect the VM state before cloning", name)
	}
	via := cl.VMExits[src.Name]
	if via == "" {
		via = cl.DefaultExit
	}
	if via == "" {
		via = "direct"
	}
	vnet, err := s.Clusters.AddVM(cl.ID, name)
	if err != nil {
		return nil, nil, err
	}
	if err := s.Clusters.SetVMExit(cl.ID, name, via); err != nil {
		if rollbackErr := s.Clusters.RemoveVM(cl.ID, name); rollbackErr != nil {
			return nil, nil, fmt.Errorf("clone route save failed (%v); membership rollback also failed (%v); no VM files were created", err, rollbackErr)
		}
		return nil, nil, fmt.Errorf("clone route save failed, membership rolled back: %w", err)
	}
	return cl, vnet, nil
}

// rollbackCloneVM removes a clone whose creation failed after the spec
// existed: maps/relays from the latest store, then the VM itself. On
// lock timeout nothing is deleted and the returned error tells the
// caller to finish with `pvm rm` (idempotent).
func (s *Server) rollbackCloneVM(cl *cluster.Cluster, spec *vm.Spec) error {
	ctx, cancel := context.WithTimeout(context.Background(), rollbackLockTimeout)
	defer cancel()
	if err := s.mapMu.Lock(ctx); err != nil {
		return fmt.Errorf("cleanup incomplete (map lock: %v); run `pvm rm %s` to finish", err, spec.Name)
	}
	defer s.mapMu.Unlock()
	// Clean up from the latest store, not just the inherited maps: a
	// map committed for the clone after the snapshot must not orphan
	// its relay.
	if err := s.instance(spec).Stop(); err != nil {
		return fmt.Errorf("cleanup could not stop VM: %w; run `pvm rm %s`", err, spec.Name)
	}
	if err := s.closeVMResources(spec.Name, cl); err != nil {
		return fmt.Errorf("cleanup could not save membership removal: %w; run `pvm rm %s`", err, spec.Name)
	}
	pushErr := s.pushBounded(cl.ID)
	linkErr := s.removeLink(cl, spec.Name)
	if err := vm.Delete(s.P, spec.Name); err != nil {
		return errors.Join(fmt.Errorf("cleanup could not remove VM files: %w; run `pvm rm %s`", err, spec.Name), pushErr, linkErr)
	}
	s.mu.Lock()
	delete(s.vms, spec.Name)
	s.mu.Unlock()
	if err := errors.Join(pushErr, linkErr); err != nil {
		return fmt.Errorf("clone removed but gateway cleanup incomplete: %w", err)
	}
	return nil
}

// cloneInheritMaps installs the clone's inherited port maps under
// mapMu through the same validation and commit path as every other map
// creation (membership, global port/proto ownership, per-map push and
// rollback). The HTTP status return matches the host map API's codes.
func (s *Server) cloneInheritMaps(ctx context.Context, spec *vm.Spec, maps []cloneMapRequest) ([]*cluster.PortMap, int, error) {
	if err := s.mapMu.Lock(ctx); err != nil {
		return nil, 500, err
	}
	defer s.mapMu.Unlock()
	var added []*cluster.PortMap
	for _, m := range maps {
		if m.GuestPort <= 0 || m.GuestPort > 65535 {
			return nil, 400, fmt.Errorf("guest_port %d out of range", m.GuestPort)
		}
		// The clone API has always required an explicit host port; 0 is
		// not auto-assigned here even though plain map add supports it.
		if m.HostPort < 1024 || m.HostPort > 65535 {
			return nil, 400, fmt.Errorf("host_port %d must be 1024-65535", m.HostPort)
		}
		pm, code, err := s.addMap(ctx, spec, m.HostPort, m.GuestPort, m.Proto, s.relays.Ensure, s.pushGatewayConfigCtx)
		if err != nil {
			return nil, code, fmt.Errorf("map %d:%d/%s: %w", m.HostPort, m.GuestPort, m.Proto, err)
		}
		added = append(added, pm)
	}
	return added, 0, nil
}

// The source's netplan pins DHCP to the source MAC, so a clone has no
// network until it is rewritten. Prefer the network-independent agent;
// readiness waits for the reset and exposes a retryable failure stage.
func (s *Server) maybeIdentityReset(spec *vm.Spec) {
	if !spec.IdentityReset {
		return
	}
	// Capture identity, not the mutable cached spec.
	name, createdAt := spec.Name, spec.CreatedAt
	go func() {
		lock := qemu.LockVM(name)
		lock.Lock()
		defer lock.Unlock()
		fresh, err := vm.Load(s.P, name)
		if err != nil || !fresh.CreatedAt.Equal(createdAt) || !fresh.IdentityReset {
			return
		}
		if err := s.identityReset(fresh); err != nil {
			log.Printf("clone %s: identity reset failed: %v", name, err)
			s.stage(name, "failed", fmt.Sprintf("identity reset failed: %v; retry `pvm start %s`", err, name))
			return
		}
		if err := s.completeIdentityReset(fresh); err != nil {
			log.Printf("clone %s: identity reset flag save: %v", name, err)
			s.stage(name, "failed", fmt.Sprintf("identity reset state save failed: %v; retry `pvm start %s`", err, name))
			return
		}
		if ready, err := vm.Load(s.P, name); err == nil {
			s.ensureAgentAsync(ready)
		}
		log.Printf("clone %s: guest identity regenerated", name)
	}()
}

// completeIdentityReset runs under the VM lock and updates only the current
// generation. A deleted or replaced VM cannot be recreated by a late task.
func (s *Server) completeIdentityReset(spec *vm.Spec) error {
	fresh, err := vm.Load(s.P, spec.Name)
	if err != nil {
		return err
	}
	if !fresh.CreatedAt.Equal(spec.CreatedAt) {
		return fmt.Errorf("VM generation changed during identity reset")
	}
	fresh.IdentityReset = false
	fresh.SSHKeyName = fresh.Name
	if err := fresh.Save(s.P); err != nil {
		return err
	}
	s.cacheSpec(fresh)
	return nil
}

func (s *Server) identityReset(spec *vm.Spec) error {
	oldPub, err := keys.Ensure(spec.SSHKeyPath(s.P))
	if err != nil {
		return fmt.Errorf("load clone bootstrap key: %w", err)
	}
	newPub, err := keys.Ensure(s.P.VMSSHKey(spec.Name))
	if err != nil {
		return fmt.Errorf("create clone SSH key: %w", err)
	}
	script := identityResetScript(spec) + cloneAuthorizedKeyScript(spec.User, oldPub, newPub)

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

	if err := s.waitSSH(spec, 6*time.Minute); err != nil {
		return fmt.Errorf("identity reset unreachable (agent: %v): %w", lastErr, err)
	}
	if out, err := s.sshRun(spec, script); err != nil {
		return fmt.Errorf("identity reset via ssh: %v\n%s", err, out)
	}
	return nil
}

// Ubuntu's ssh restart keeps the current session alive. Netplan matches
// the NIC by name rather than the source MAC so later clones also boot
// with networking. Only machine-level identifiers are reset; user files
// are inherited verbatim.
// Preserve user-managed keys while replacing the inherited pvm key.
func cloneAuthorizedKeyScript(user, oldPub, newPub string) string {
	oldFields := strings.Fields(oldPub)
	newFields := strings.Fields(newPub)
	script := fmt.Sprintf(
		"set -e\nauth=%s\ntmp=$(mktemp \"$auth.pvm-XXXXXX\")\ntrap 'rm -f \"$tmp\"' EXIT\n"+
			"awk -v old=%s -v new=%s '{ keep=1; for(i=1;i<=NF;i++) if($i==old || $i==new) keep=0; if(keep) print }' \"$auth\" > \"$tmp\"\n"+
			"printf '%%s\\n' %s >> \"$tmp\"\n"+
			"chown --reference=\"$auth\" \"$tmp\"\nchmod 600 \"$tmp\"\nmv -f \"$tmp\" \"$auth\"\n",
		shellQuote("/home/"+user+"/.ssh/authorized_keys"), shellQuote(oldFields[1]), shellQuote(newFields[1]), shellQuote(strings.TrimSpace(newPub)))
	return "$SUDO sh -c " + shellQuote(script) + "\n"
}

func identityResetScript(spec *vm.Spec) string {
	return ubuntuIdentityResetScript(spec.Name)
}

// useSudo expands to `sudo` only when the caller is not root: the agent
// channel already runs as root, while the ssh fallback is an unprivileged
// user that needs it.
const useSudo = `SUDO=''; [ "$(id -u)" -ne 0 ] && SUDO='sudo'`

func ubuntuIdentityResetScript(newHostname string) string {
	// set -e makes failures abort non-zero so the caller retries on the next
	// boot; the tolerated ssh restart cannot mask earlier failures.
	return "set -e\n" + useSudo + "\n" + fmt.Sprintf(
		"printf 'network:\n  version: 2\n  ethernets:\n    primary:\n      match:\n        name: en*\n      dhcp4: true\n' | $SUDO tee /etc/netplan/50-cloud-init.yaml >/dev/null\n"+
			"$SUDO netplan apply\n"+
			"$SUDO hostnamectl set-hostname %[1]s\n"+
			"$SUDO sh -c 'rm -f /etc/machine-id /var/lib/dbus/machine-id && systemd-machine-id-setup'\n"+
			"$SUDO rm -f /etc/ssh/ssh_host_* && $SUDO ssh-keygen -A\n"+
			"($SUDO systemctl restart ssh.socket ssh.service 2>/dev/null || true)\n",
		newHostname)
}
