package daemon

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/cluster"
	"github.com/huangzheng2016/pvm/internal/qemu"
	"github.com/huangzheng2016/pvm/internal/vm"
)

// gatewayAgentBinary returns the unpacked guest agent for the host arch.
func (s *Server) gatewayAgentBinary() (string, error) {
	arch := runtime.GOARCH
	p := filepath.Join(s.P.Runtime, "gateway-linux-"+arch)
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
	gwName := cl.Gateway
	spec, err := vm.Load(s.P, gwName)
	if err != nil {
		spec, err = s.createGatewayVM(cl)
		if err != nil {
			return nil, fmt.Errorf("create gateway vm: %w", err)
		}
	}
	inst := s.instance(spec)
	if inst.Running() == nil {
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
		if err != nil || s.instance(spec).Running() == nil {
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
	if err := q.DeviceDel("nic0"); err != nil {
		return fmt.Errorf("device_del: %w", err)
	}
	if err := q.WaitEvent("DEVICE_DELETED", 10*time.Second); err != nil {
		return fmt.Errorf("wait unplug: %w", err)
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
	base := filepath.Join(s.P.Images, fmt.Sprintf("ubuntu-24.04-%s.qcow2", runtime.GOARCH))
	if _, err := os.Stat(base); err != nil {
		return nil, fmt.Errorf("gateway needs the ubuntu:24.04 base image; run `pvm image fetch ubuntu:24.04`")
	}
	spec := &vm.Spec{
		Name: cl.Gateway, Image: "ubuntu:24.04", Role: "gateway",
		User: "pvmgw", GuestUID: os.Getuid(), Arch: runtime.GOARCH,
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
	if gwInst.Running() == nil {
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
	if s.instance(gwSpec).Running() == nil {
		return nil
	}
	q, err := qemu.DialQMP(filepath.Join(s.P.VMDir(cl.Gateway), "qmp.sock"))
	if err != nil {
		return nil
	}
	defer q.Close()
	q.DeviceDel("nic-" + vmName)
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
	if !s.waitSSH(gwSpec, 180*time.Second) {
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
	if err := s.pushGatewayConfig(cl); err != nil {
		return err
	}
	if out, err := s.sshRun(gwSpec, "sudo systemctl restart pvm-gateway.service"); err != nil {
		return fmt.Errorf("start agent: %v\n%s", err, out)
	}
	log.Printf("gateway %s: agent deployed", cl.Gateway)
	return nil
}

// pushGatewayConfig renders gateway.json from the cluster store and
// writes it into the gateway guest.
func (s *Server) pushGatewayConfig(cl *cluster.Cluster) error {
	gwSpec, err := s.getSpec(cl.Gateway)
	if err != nil {
		return err
	}
	cfg := gatewayConfigFromCluster(cl)
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
	if err := s.scpTo(gwSpec, tmp.Name(), "/tmp/gateway.json"); err != nil {
		return err
	}
	out, err := s.sshRun(gwSpec, "sudo install -m 0644 /tmp/gateway.json /etc/pvm/gateway.json")
	if err != nil {
		return fmt.Errorf("install config: %v\n%s", err, out)
	}
	return nil
}

func (s *Server) sshBaseArgs(spec *vm.Spec) []string {
	return []string{
		"-i", s.P.SSHKey(),
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=5",
		"-o", "ConnectionAttempts=1",
		"-p", strconv.Itoa(spec.SSHPort),
	}
}

func (s *Server) sshRun(spec *vm.Spec, cmd string) (string, error) {
	args := append(s.sshBaseArgs(spec), fmt.Sprintf("%s@127.0.0.1", spec.User), cmd)
	out, err := exec.Command("ssh", args...).CombinedOutput()
	return string(out), err
}

func (s *Server) scpTo(spec *vm.Spec, local, remote string) error {
	base := s.sshBaseArgs(spec)
	base[len(base)-2] = "-P" // scp spells the port flag with a capital
	args := append(base, local, fmt.Sprintf("%s@127.0.0.1:%s", spec.User, remote))
	out, err := exec.Command("scp", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("scp: %v\n%s", err, out)
	}
	return nil
}

func (s *Server) waitSSH(spec *vm.Spec, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := s.sshRun(spec, "true"); err == nil {
			return true
		}
		time.Sleep(3 * time.Second)
	}
	return false
}
