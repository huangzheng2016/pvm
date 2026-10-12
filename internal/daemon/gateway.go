package daemon

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/huangzheng2016/pvm/internal/cluster"
	"github.com/huangzheng2016/pvm/internal/native"
	"github.com/huangzheng2016/pvm/internal/qemu"
	"github.com/huangzheng2016/pvm/internal/vm"
)

// gatewayAgentBinary returns the unpacked native gateway agent.
func (s *Server) gatewayAgentBinary() (string, error) {
	p := filepath.Join(s.P.Runtime, native.GuestBinary("gateway"))
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("gateway agent %s not unpacked; rebuild with script/build.sh", p)
	}
	return p, nil
}

// ensureCluster makes sure the cluster exists in the store and its
// gateway VM exists and is running, with the agent deployed. It is safe
// to call repeatedly.
func (s *Server) ensureCluster(id int) (*cluster.Cluster, error) {
	cl, err := s.Clusters.Ensure(id)
	if err != nil {
		return nil, err
	}
	// Gateway lifecycle is serialised too: two user VMs racing their
	// first start (or an API start racing daemon recovery) would both
	// try to create/start the same gateway.
	lock := qemu.LockVM(cl.Gateway)
	lock.Lock()
	defer lock.Unlock()
	spec, err := vm.Load(s.P, cl.Gateway)
	if err != nil {
		spec, err = s.createGatewayVM(cl)
		if err != nil {
			return nil, fmt.Errorf("create gateway vm: %w", err)
		}
	}
	inst := s.instance(spec)
	if st, err := inst.Running(); err != nil {
		return nil, err
	} else if st == nil {
		if err := inst.Start(); err != nil {
			return nil, fmt.Errorf("start gateway: %w", err)
		}
		go s.onGatewayStarted(cl)
	}
	return cl, nil
}

// onGatewayStarted re-establishes cluster links after a gateway (re)
// start: gateway-side sockets for every VM, NIC hot-replace on running
// user VMs, agent deploy, then a reachability check with one repair
// retry for VMs whose link did not come back.
func (s *Server) onGatewayStarted(cl *cluster.Cluster) {
	var repaired []*vm.Spec
	for _, vn := range cl.VMs {
		if err := s.ensureLink(cl, vn.Name); err != nil {
			log.Printf("re-link %s: %v", vn.Name, err)
			continue
		}
		spec, err := s.getSpec(vn.Name)
		if err != nil {
			continue
		}
		if st, rerr := s.instance(spec).Running(); rerr != nil {
			log.Printf("inspect vm %s state: %v", vn.Name, rerr)
			continue
		} else if st == nil {
			continue
		}
		if err := s.repairClientLink(cl, spec); err != nil {
			log.Printf("repair nic %s: %v", vn.Name, err)
			continue
		}
		repaired = append(repaired, spec)
	}
	if err := s.deployGatewayAgent(cl); err != nil {
		log.Printf("deploy gateway agent: %v", err)
		return
	}
	for _, spec := range repaired {
		vn := cl.VMs[spec.Name]
		if vn == nil {
			continue
		}
		if s.waitPingFromGateway(cl, vn.IP, 60*time.Second) {
			continue
		}
		log.Printf("vm %s unreachable after gateway restart, retrying nic repair", spec.Name)
		if err := s.repairClientLink(cl, spec); err != nil {
			log.Printf("retry repair %s: %v", spec.Name, err)
			continue
		}
		if s.waitPingFromGateway(cl, vn.IP, 60*time.Second) {
			log.Printf("vm %s: link recovered on retry", spec.Name)
		} else {
			log.Printf("vm %s: still unreachable; `pvm stop %s && pvm start %s`", spec.Name, spec.Name, spec.Name)
		}
	}
}

func (s *Server) pingFromGateway(cl *cluster.Cluster, ip string) bool {
	gwSpec, err := s.getSpec(cl.Gateway)
	if err != nil {
		return false
	}
	_, err = s.sshRun(gwSpec, "ping -c1 -W2 "+ip)
	return err == nil
}

func (s *Server) waitPingFromGateway(cl *cluster.Cluster, ip string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if s.pingFromGateway(cl, ip) {
			return true
		}
		time.Sleep(4 * time.Second)
	}
	return false
}

// repairClientLink hot-replaces the cluster NIC of a running user VM:
// the old one is still bound to a dead stream socket. Same MAC, so the
// guest re-DHCPs into its previous fixed IP. device_del is asynchronous
// — wait for DEVICE_DELETED before re-adding, otherwise the re-add
// races the pending unplug and silently no-ops.
func (s *Server) repairClientLink(cl *cluster.Cluster, spec *vm.Spec) error {
	lock := qemu.LockVM(spec.Name)
	lock.Lock()
	defer lock.Unlock()
	q, err := qemu.DialQMP(filepath.Join(s.P.VMDir(spec.Name), "qmp.sock"))
	if err != nil {
		return fmt.Errorf("qmp: %w", err)
	}
	defer q.Close()
	if err := q.DeviceDel("nic0", 10*time.Second); err != nil {
		return fmt.Errorf("device_del: %w", err)
	}
	if err := q.NetdevDel("net0"); err != nil {
		return fmt.Errorf("netdev_del: %w", err)
	}
	sock := s.Clusters.LinkSock(cl.ID, spec.Name)
	if err := q.NetdevAddStreamConnect("net0", sock); err != nil {
		return fmt.Errorf("netdev_add: %w", err)
	}
	if err := q.DeviceAddNet("nic0", "net0", spec.MAC, "rp-link"); err != nil {
		return fmt.Errorf("device_add: %w", err)
	}
	log.Printf("vm %s: cluster nic repaired", spec.Name)
	return nil
}

func (s *Server) createGatewayVM(cl *cluster.Cluster) (*vm.Spec, error) {
	port, err := vm.AllocSSHPort(vm.UsedSSHPorts(s.P), vm.SSHPortBase)
	if err != nil {
		return nil, err
	}
	// The gateway is distro-agnostic inside; boot it off the newest cached
	// ubuntu base image instead of pinning a version the user may not have.
	base, imgSpec := "", ""
	for _, v := range []string{"26.04", "24.04", "22.04"} {
		cand := filepath.Join(s.P.Images, fmt.Sprintf("ubuntu-%s-%s.qcow2", v, native.Arch))
		if _, err := os.Stat(cand); err == nil {
			base, imgSpec = cand, "ubuntu:"+v
			break
		}
	}
	if base == "" {
		return nil, fmt.Errorf("gateway needs an ubuntu base image; run `pvm image fetch ubuntu:26.04`")
	}
	spec := &vm.Spec{
		Name: cl.Gateway, Image: imgSpec, Role: "gateway",
		User: "pvmgw", GuestUID: os.Getuid(),
		CPUs: 1, MemMB: 512, DiskGB: 10,
		SSHPort: port, BaseDisk: base, ClusterID: cl.ID,
	}
	if err := vm.Create(s.P, s.TC, spec, s.SSHPub); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.vms[spec.Name] = spec
	s.mu.Unlock()
	return spec, nil
}

// ensureLink hot-adds the cluster link (stream netdev + virtio NIC) on
// the gateway for the given user VM. The gateway QEMU must be running.
func (s *Server) ensureLink(cl *cluster.Cluster, vmName string) error {
	gwSpec, err := s.getSpec(cl.Gateway)
	if err != nil {
		return err
	}
	gwInst := s.instance(gwSpec)
	if st, err := gwInst.Running(); err != nil {
		return err
	} else if st == nil {
		return fmt.Errorf("gateway %s not running", cl.Gateway)
	}
	q, err := qemu.DialQMP(filepath.Join(s.P.VMDir(cl.Gateway), "qmp.sock"))
	if err != nil {
		return fmt.Errorf("gateway qmp: %w", err)
	}
	defer q.Close()
	if err := os.MkdirAll(s.Clusters.LinksDir(cl.ID), 0o700); err != nil {
		return err
	}
	sock := s.Clusters.LinkSock(cl.ID, vmName)
	// no os.Remove here: qemu unlinks the path itself when binding a
	// fresh listener, and removing a LIVE listener's path would break
	// future client connects.
	vn := cl.VMs[vmName]
	if vn == nil {
		return fmt.Errorf("vm %s has no cluster net entry", vmName)
	}
	bus := "rp" + vn.IP[strings.LastIndex(vn.IP, ".")+1:]
	netdevID := "link-" + vmName
	if err := q.NetdevAddStream(netdevID, sock); err != nil {
		return fmt.Errorf("netdev_add: %w", err)
	}
	err = q.DeviceAddNet("nic-"+vmName, netdevID, "", bus)
	if err != nil && strings.Contains(err.Error(), "uplicate") {
		// the link is already fully hot-plugged (e.g. onGatewayStarted
		// just rebuilt it) — that is exactly the state we want.
		return nil
	}
	if err != nil {
		return fmt.Errorf("device_add: %w", err)
	}
	return nil
}

func (s *Server) removeLink(cl *cluster.Cluster, vmName string) error {
	gwSpec, err := s.getSpec(cl.Gateway)
	if err != nil {
		return nil // gateway gone already
	}
	if st, err := s.instance(gwSpec).Running(); err != nil || st == nil {
		return nil
	}
	q, err := qemu.DialQMP(filepath.Join(s.P.VMDir(cl.Gateway), "qmp.sock"))
	if err != nil {
		return nil
	}
	defer q.Close()
	q.DeviceDel("nic-"+vmName, 5*time.Second) // best-effort cleanup
	q.NetdevDel("link-" + vmName)
	return nil
}

// deployGatewayAgent waits for the gateway ssh to come up, pushes the
// agent binary and config, and starts the service.
func (s *Server) deployGatewayAgent(cl *cluster.Cluster) error {
	gwSpec, err := s.getSpec(cl.Gateway)
	if err != nil {
		return err
	}
	if err := s.waitSSH(gwSpec, 180*time.Second); err != nil {
		return fmt.Errorf("gateway %s ssh not ready after 3min", cl.Gateway)
	}
	// sshd wins the race against cloud-init; /etc/pvm and the systemd
	// unit only exist once runcmd finishes.
	if _, err := s.sshRun(gwSpec, "sudo cloud-init status --wait >/dev/null 2>&1 || true"); err != nil {
		return fmt.Errorf("cloud-init wait: %w", err)
	}
	bin, err := s.gatewayAgentBinary()
	if err != nil {
		return err
	}
	if err := s.scpTo(gwSpec, bin, "/tmp/pvm-gateway"); err != nil {
		return fmt.Errorf("scp agent: %w", err)
	}
	if out, err := s.sshRun(gwSpec, "sudo install -m 0755 /tmp/pvm-gateway /usr/local/bin/pvm-gateway"); err != nil {
		return fmt.Errorf("install agent: %v\n%s", err, out)
	}
	if err := s.pushGatewayConfig(cl.ID); err != nil {
		return err
	}
	if out, err := s.sshRun(gwSpec, "sudo systemctl restart pvm-gateway.service"); err != nil {
		return fmt.Errorf("start agent: %v\n%s", err, out)
	}
	log.Printf("gateway %s: agent deployed", cl.Gateway)
	return nil
}

// pushGatewayConfig renders gateway.json from a FRESH cluster snapshot
// and writes it into the gateway guest. Callers pass only the cluster
// ID: rendering re-reads the store at push time, so a stale caller-held
// copy can never roll back someone else's newer config. All pushes for
// one gateway are serialized through a per-cluster mutex, and the guest
// file is replaced atomically (install to temp + mv), so concurrent
// callers cannot interleave a truncated file.
func (s *Server) pushGatewayConfig(clusterID int) error {
	cl := s.Clusters.Get(clusterID)
	if cl == nil {
		return fmt.Errorf("cluster %d not found", clusterID)
	}
	gwSpec, err := s.getSpec(cl.Gateway)
	if err != nil {
		return err
	}
	return s.renderGatewayConfigLocked(clusterID, func(cfg []byte) error {
		tmp, err := os.CreateTemp("", "pvm-gwcfg-*.json")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		if _, err := tmp.Write(cfg); err != nil {
			tmp.Close()
			return err
		}
		tmp.Close()
		remote := gatewayRemoteTmp()
		if err := s.scpTo(gwSpec, tmp.Name(), remote); err != nil {
			return err
		}
		out, err := s.sshRun(gwSpec, gatewayInstallCmd(remote))
		if err != nil {
			return fmt.Errorf("install config: %v\n%s", err, out)
		}
		return nil
	})
}

// renderGatewayConfigLocked serializes config renders for one cluster
// and invokes fn with a payload rendered from a fresh store snapshot.
// Callers must not hold any cluster-derived state across fn — the
// point is that everything fn sends reflects the store as of this call.
func (s *Server) renderGatewayConfigLocked(clusterID int, fn func([]byte) error) error {
	mu, _ := s.gwPushLocks.LoadOrStore(clusterID, &sync.Mutex{})
	lock := mu.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	cl := s.Clusters.Get(clusterID)
	if cl == nil {
		return fmt.Errorf("cluster %d not found", clusterID)
	}
	return fn(gatewayConfigFromCluster(s, cl))
}

// gatewayRemoteTmp is the per-push unique scp target on the gateway:
// two pushes can never scp over each other's half-written file.
func gatewayRemoteTmp() string {
	return fmt.Sprintf("/tmp/gateway.json.pvm-%d", time.Now().UnixNano())
}

// gatewayInstallCmd atomically moves the freshly uploaded config into
// place (same-filesystem rename) and cleans up the upload.
func gatewayInstallCmd(remote string) string {
	return fmt.Sprintf("sudo install -m 0644 %s /etc/pvm/gateway.json.pvm-new && sudo mv -f /etc/pvm/gateway.json.pvm-new /etc/pvm/gateway.json; rc=$?; rm -f %s; exit $rc", remote, remote)
}

// pushGatewayConfigRetry pushes the gateway config, retrying while the
// gateway's ssh is still coming up after a (re)start. Each attempt
// re-renders from a fresh snapshot and aborts immediately if the
// gateway qemu is dead instead of blocking for the full budget.
func (s *Server) pushGatewayConfigRetry(clusterID int) error {
	var err error
	gwName := ""
	for i := 0; i < 60; i++ {
		cl := s.Clusters.Get(clusterID)
		if cl == nil {
			return fmt.Errorf("cluster %d not found", clusterID)
		}
		gwName = cl.Gateway
		gwSpec, gerr := s.getSpec(cl.Gateway)
		if gerr != nil {
			return gerr
		}
		if st, rerr := s.instance(gwSpec).Running(); rerr != nil || st == nil {
			return fmt.Errorf("gateway %s is not running; cannot push config: %v", cl.Gateway, rerr)
		}
		if err = s.pushGatewayConfig(clusterID); err == nil {
			return nil
		}
		time.Sleep(3 * time.Second)
	}
	return fmt.Errorf("push gateway config: %w (gateway %s may still be initializing)", err, gwName)
}

// sshBaseArgs builds the ssh arguments and target for a guest. Cluster
// VMs have no hostfwd: they are reached through the gateway hop, the
// same ProxyCommand path `pvm ssh` uses.
func (s *Server) sshBaseArgs(spec *vm.Spec) ([]string, string, error) {
	args := []string{
		"-i", s.P.SSHKey(),
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=5",
		"-o", "ConnectionAttempts=1",
		"-o", "BatchMode=yes",
	}
	if spec.Role == "gateway" || spec.ClusterID == 0 {
		return append(args, "-p", strconv.Itoa(spec.SSHPort)), fmt.Sprintf("%s@127.0.0.1", spec.User), nil
	}
	cl := s.Clusters.Get(spec.ClusterID)
	if cl == nil {
		return nil, "", fmt.Errorf("cluster %d not found for vm %s", spec.ClusterID, spec.Name)
	}
	gw, err := s.getSpec(cl.Gateway)
	if err != nil {
		return nil, "", err
	}
	args = append(args, "-o", fmt.Sprintf("ProxyCommand=ssh -i %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -W %%h:%%p -p %d %s@127.0.0.1",
		s.P.SSHKey(), gw.SSHPort, gw.User))
	return args, fmt.Sprintf("%s@%s", spec.User, spec.IP), nil
}

// sshRun executes cmd in the guest over the gateway jump. Only stdout is
// returned on success: banners and remote-shell warnings land on stderr
// and would otherwise corrupt machine-parsed output (json, version
// strings, region profiles). Failures include stderr for diagnosis.
func (s *Server) sshRun(spec *vm.Spec, cmd string) (string, error) {
	args, target, err := s.sshBaseArgs(spec)
	if err != nil {
		return "", err
	}
	c := exec.Command("ssh", append(args, target, cmd)...)
	var stdout, stderr strings.Builder
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func (s *Server) scpTo(spec *vm.Spec, local, remote string) error {
	base, target, err := s.sshBaseArgs(spec)
	if err != nil {
		return err
	}
	for i, a := range base {
		if a == "-p" { // scp spells the port flag with a capital
			base[i] = "-P"
		}
	}
	args := append(base, local, target+":"+remote)
	out, err := exec.Command("scp", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("scp: %v\n%s", err, out)
	}
	return nil
}
