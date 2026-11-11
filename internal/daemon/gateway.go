package daemon

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/cluster"
	"github.com/huangzheng2016/pvm/internal/fsutil"
	"github.com/huangzheng2016/pvm/internal/native"
	"github.com/huangzheng2016/pvm/internal/qemu"
	"github.com/huangzheng2016/pvm/internal/vm"
)

func (s *Server) gatewayAgentBinary() (string, error) {
	p := filepath.Join(s.P.Runtime, native.GuestBinary("gateway"))
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("gateway agent %s not unpacked; rebuild with script/build.sh", p)
	}
	return p, nil
}

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

// ensureGatewayKernelArgs cold-boots the gateway when the running kernel
// lacks spec.KernelArgs: cloud-init installs them into grub during the
// first boot, which itself still runs without them, and -no-reboot
// guests cannot restart themselves. The caller holds the gateway deploy
// gate, so the restarted gateway's own deploy waits for this one.
func (s *Server) ensureGatewayKernelArgs(ctx context.Context, gw *vm.Spec) (bool, error) {
	// /etc/pvm and the grub drop-in only exist once runcmd finishes.
	if out, err := s.sshRunCtx(ctx, gw, "sudo cloud-init status --wait"); err != nil {
		return false, fmt.Errorf("cloud-init wait: %v%s", err, indentOutput(out))
	}
	st, err := s.instance(gw).Running()
	if err != nil {
		return false, err
	}
	if st == nil {
		return false, fmt.Errorf("gateway %s not running", gw.Name)
	}
	cmdline, err := s.sshRunCtx(ctx, gw, "cat /proc/cmdline")
	if err != nil {
		return false, fmt.Errorf("read guest kernel cmdline: %w", err)
	}
	have := strings.Fields(cmdline)
	missing := false
	check := "sync"
	for _, arg := range strings.Fields(gw.KernelArgs) {
		if !slices.Contains(have, arg) {
			missing = true
		}
		check += " && sudo grep -qwF -- '" + arg + "' /boot/grub/grub.cfg"
	}
	if !missing {
		return false, nil
	}
	if out, err := s.sshRunCtx(ctx, gw, check); err != nil {
		return false, fmt.Errorf("guest grub lacks kernel args %q: %v\n%s", gw.KernelArgs, err, out)
	}
	lock := qemu.LockVM(gw.Name)
	lock.Lock()
	defer lock.Unlock()
	inst := s.instance(gw)
	cur, err := inst.Running()
	if err != nil {
		return false, err
	}
	if cur == nil || cur.PID != st.PID {
		return false, fmt.Errorf("gateway %s changed state during kernel-args check", gw.Name)
	}
	log.Printf("gateway %s: cold boot to apply guest kernel args %q", gw.Name, gw.KernelArgs)
	if err := inst.Stop(); err != nil {
		return false, fmt.Errorf("stop gateway for kernel args: %w", err)
	}
	if _, _, err := s.startVM(ctx, gw); err != nil {
		return false, fmt.Errorf("restart gateway for kernel args: %w", err)
	}
	return true, nil
}

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
	s.deployClusterAgent(cl)
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
	q, err := qemu.DialQMP(s.P.QMPSock(spec.Name))
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
	usedPorts, err := vm.UsedSSHPorts(s.P)
	if err != nil {
		return nil, err
	}
	port, err := vm.AllocSSHPort(usedPorts, vm.SSHPortBase)
	if err != nil {
		return nil, err
	}
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
		User: "pvmgw", GuestUID: vm.HostUID(),
		// Current Ubuntu initramfs decompression exceeds a 512 MiB guest.
		CPUs: 1, MemMB: 1024, DiskGB: 10,
		SSHPort: port, BaseDisk: base, ClusterID: cl.ID,
		KernelArgs: s.Accel.GuestKernelArgs(),
	}
	if err := vm.Create(s.P, s.TC, spec); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.vms[spec.Name] = copySpec(spec)
	s.mu.Unlock()
	return spec, nil
}

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
	q, err := qemu.DialQMP(s.P.QMPSock(cl.Gateway))
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

func (s *Server) ensureLinkRetry(ctx context.Context, cl *cluster.Cluster, vmName string) error {
	var lastErr error
	for i := 0; i < gatewayRetryAttempts; i++ {
		if err := s.ensureLink(cl, vmName); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if err := waitInstallRetry(ctx, gatewayRetryDelay); err != nil {
			return fmt.Errorf("ensure link: %w (last error: %v)", err, lastErr)
		}
	}
	return fmt.Errorf("ensure link: %w", lastErr)
}

func (s *Server) removeLink(cl *cluster.Cluster, vmName string) error {
	gwSpec, err := s.getSpec(cl.Gateway)
	if err != nil {
		return nil
	}
	if st, err := s.instance(gwSpec).Running(); err != nil || st == nil {
		return nil
	}
	q, err := qemu.DialQMP(s.P.QMPSock(cl.Gateway))
	if err != nil {
		return nil
	}
	defer q.Close()
	q.DeviceDel("nic-"+vmName, 5*time.Second) // best-effort cleanup
	q.NetdevDel("link-" + vmName)
	return nil
}

func (s *Server) deployGatewayAgentCtx(ctx context.Context, cl *cluster.Cluster) error {
	v, _ := s.gwAgentLocks.LoadOrStore(cl.ID, make(chan struct{}, 1))
	gate := v.(chan struct{})
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	gwSpec, err := s.getSpec(cl.Gateway)
	if err != nil {
		return err
	}
	bin, err := s.gatewayAgentBinary()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(bin)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	localSHA := hex.EncodeToString(digest[:])
	if err := s.waitSSHCtx(ctx, gwSpec, 6*time.Minute); err != nil {
		return fmt.Errorf("gateway %s ssh: %w", cl.Gateway, err)
	}
	if gwSpec.KernelArgs != "" {
		restarted, err := s.ensureGatewayKernelArgs(ctx, gwSpec)
		if err != nil {
			return err
		}
		if restarted {
			if err := s.waitSSHCtx(ctx, gwSpec, 6*time.Minute); err != nil {
				return fmt.Errorf("gateway %s ssh after kernel-args cold boot: %w", cl.Gateway, err)
			}
		}
	}
	out, err := s.sshRunCtx(ctx, gwSpec, "if systemctl is-active --quiet pvm-gateway.service && test -x /usr/local/bin/pvm-gateway; then sha256sum /usr/local/bin/pvm-gateway; fi")
	if err != nil {
		return fmt.Errorf("inspect gateway agent: %w", err)
	}
	if fields := strings.Fields(out); len(fields) > 0 && fields[0] == localSHA {
		return nil
	}
	// sshd wins the race against cloud-init; /etc/pvm and the systemd
	// unit only exist once runcmd finishes. Slow TCG/cloud mirrors can
	// take several minutes, so do not kill a valid provisioning run at 2min.
	if out, err := s.sshRunCtx(ctx, gwSpec, "sudo cloud-init status --wait"); err != nil {
		return fmt.Errorf("cloud-init wait: %v%s", err, indentOutput(out))
	}
	if err := s.scpToCtx(ctx, gwSpec, bin, "/tmp/pvm-gateway"); err != nil {
		return fmt.Errorf("scp agent: %w", err)
	}
	if out, err := s.sshRunCtx(ctx, gwSpec, "sudo install -m 0755 /tmp/pvm-gateway /usr/local/bin/pvm-gateway"); err != nil {
		return fmt.Errorf("install agent: %v\n%s", err, out)
	}
	if err := s.pushGatewayConfigCtx(ctx, cl.ID); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if out, err := s.sshRunCtx(ctx, gwSpec, "sudo systemctl restart pvm-gateway.service"); err != nil {
		return fmt.Errorf("start agent: %v\n%s", err, out)
	}
	log.Printf("gateway %s: agent deployed", cl.Gateway)
	return nil
}

// pushGatewayConfig renders gateway.json from a FRESH cluster snapshot
// and writes it into the gateway guest. Callers pass only the cluster
// ID: rendering re-reads the store at push time, so a stale caller-held
// copy can never roll back someone else's newer config. All pushes for
// one gateway are serialized through a per-cluster gate, and the guest
// file is replaced atomically (install to temp + mv), so concurrent
// callers cannot interleave a truncated file.
func (s *Server) pushGatewayConfig(clusterID int) error {
	return s.pushGatewayConfigCtx(context.Background(), clusterID)
}

// pushGatewayConfigCtx is the context-aware variant: a cancelled ctx
// aborts the gate wait and kills the scp/ssh children, so guest control
// deadlines and disconnects actually stop the work.
func (s *Server) pushGatewayConfigCtx(ctx context.Context, clusterID int) error {
	cl := s.Clusters.Get(clusterID)
	if cl == nil {
		return fmt.Errorf("cluster %d not found", clusterID)
	}
	gwSpec, err := s.getSpec(cl.Gateway)
	if err != nil {
		return err
	}
	return s.renderGatewayConfigLocked(ctx, clusterID, func(cfg []byte) error {
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
		uploadDir := gatewayRemoteTmp() + ".d"
		if _, err := s.sshRunCtx(ctx, gwSpec, "mkdir -m 0700 "+shellQuote(uploadDir)); err != nil {
			return fmt.Errorf("prepare private gateway upload: %w", err)
		}
		remote := uploadDir + "/gateway.json"
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if _, err := s.sshRunCtx(cleanupCtx, gwSpec, "rm -f "+shellQuote(remote)+"; rmdir "+shellQuote(uploadDir)); err != nil {
				log.Printf("gateway config: private upload cleanup failed: %v", err)
			}
		}()
		if err := s.scpToCtx(ctx, gwSpec, tmp.Name(), remote); err != nil {
			return err
		}
		out, err := s.sshRunCtx(ctx, gwSpec, gatewayInstallCmd(remote))
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
func (s *Server) renderGatewayConfigLocked(ctx context.Context, clusterID int, fn func([]byte) error) error {
	v, _ := s.gwPushLocks.LoadOrStore(clusterID, make(chan struct{}, 1))
	gate := v.(chan struct{})
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		return ctx.Err()
	}

	cl := s.Clusters.Get(clusterID)
	if cl == nil {
		return fmt.Errorf("cluster %d not found", clusterID)
	}
	return fn(gatewayConfigFromCluster(s, cl))
}

// gatewayRemoteTmp is the per-push unique scp target on the gateway:
// two pushes can never scp over each other's half-written file.
func gatewayRemoteTmp() string {
	return "/tmp/gateway.json.pvm-" + rand.Text()
}

// gatewayInstallCmd atomically moves the freshly uploaded config into
// place (same-filesystem rename) and cleans up the upload.
func gatewayInstallCmd(remote string) string {
	return fmt.Sprintf("sudo install -d -m 0700 -o root -g root /etc/pvm && sudo install -m 0600 -o root -g root %s /etc/pvm/gateway.json.pvm-new && sudo mv -f /etc/pvm/gateway.json.pvm-new /etc/pvm/gateway.json; rc=$?; rm -f %s; exit $rc", remote, remote)
}

var gatewayRetryAttempts = 60
var gatewayRetryDelay = 3 * time.Second

// Retry while the gateway and its control channel are starting.
func (s *Server) pushGatewayConfigRetry(clusterID int) error {
	return s.pushGatewayConfigRetryCtx(context.Background(), clusterID)
}

func (s *Server) pushGatewayConfigRetryCtx(ctx context.Context, clusterID int) error {
	var err error
	for i := 0; i < gatewayRetryAttempts; i++ {
		if err = s.pushGatewayConfigCtx(ctx, clusterID); err == nil {
			return nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fmt.Errorf("push gateway config: %w", ctxErr)
		}
		if waitErr := waitInstallRetry(ctx, gatewayRetryDelay); waitErr != nil {
			return fmt.Errorf("push gateway config: %w (last error: %v)", waitErr, err)
		}
	}
	cl := s.Clusters.Get(clusterID)
	gwName := ""
	if cl != nil {
		gwName = cl.Gateway
	}
	return fmt.Errorf("push gateway config: %w (gateway %s may still be initializing)", err, gwName)
}

// sshBaseArgs builds the ssh arguments and target for a guest. Cluster
// VMs have no hostfwd: they are reached through the gateway hop, the
// same ProxyCommand path `pvm ssh` uses.
func (s *Server) sshBaseArgs(spec *vm.Spec) ([]string, string, error) {
	args := []string{
		"-F", "/dev/null",
		"-o", "IdentitiesOnly=yes",
		"-o", "IdentityAgent=none",
		"-o", "ForwardAgent=no",
		"-i", spec.SSHKeyPath(s.P),
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		"-o", "ConnectTimeout=10",
		"-o", "ConnectionAttempts=1",
		"-o", "BatchMode=yes",
	}
	// Identity reset may have installed the clone key before its spec
	// write failed. Offer that known recovery key only while reset is pending.
	if spec.IdentityReset && spec.SSHKeyName != spec.Name {
		if fi, err := os.Stat(s.P.VMSSHKey(spec.Name)); err == nil && fi.Mode().IsRegular() {
			args = append(args, "-i", s.P.VMSSHKey(spec.Name))
		}
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
	args = append(args, "-o", fmt.Sprintf("ProxyCommand=ssh -F /dev/null -o IdentitiesOnly=yes -o IdentityAgent=none -o ForwardAgent=no -i %s -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -W %%h:%%p -p %d %s@127.0.0.1",
		vm.ProxyCommandQuote(gw.SSHKeyPath(s.P)), gw.SSHPort, gw.User))
	return args, fmt.Sprintf("%s@%s", spec.User, spec.IP), nil
}

// sshRun executes cmd in the guest over the gateway jump. Only stdout is
// returned on success: banners and remote-shell warnings land on stderr
// and would otherwise corrupt machine-parsed output (json, version
// strings, region profiles). Failures include stderr and preserve stdout for diagnosis.
func (s *Server) sshRun(spec *vm.Spec, cmd string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return s.sshRunCtx(ctx, spec, cmd)
}

// sshRunCtx kills the ssh child when ctx is done, so a cancelled
// request does not leave the process running.
func (s *Server) sshRunCtx(ctx context.Context, spec *vm.Spec, cmd string) (string, error) {
	args, target, err := s.sshBaseArgs(spec)
	if err != nil {
		return "", err
	}
	c := exec.CommandContext(ctx, "ssh", append(args, target, cmd)...)
	fsutil.ConfigureHiddenCmd(c)
	c.WaitDelay = 2 * time.Second
	var stdout, stderr strings.Builder
	c.Stdout = &stdout
	c.Stderr = &stderr
	if err := c.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func (s *Server) scpTo(spec *vm.Spec, local, remote string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return s.scpToCtx(ctx, spec, local, remote)
}

func (s *Server) scpToCtx(ctx context.Context, spec *vm.Spec, local, remote string) error {
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
	c := exec.CommandContext(ctx, "scp", args...)
	fsutil.ConfigureHiddenCmd(c)
	c.WaitDelay = 2 * time.Second
	out, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("scp: %v\n%s", err, out)
	}
	return nil
}
