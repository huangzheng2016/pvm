// Package daemon is the always-resident control plane: HTTP/JSON over a
// unix socket, owning all VM and cluster lifecycle. CLI commands are
// thin clients.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/huangzheng2016/pvm/internal/agent"
	"github.com/huangzheng2016/pvm/internal/buildinfo"
	"github.com/huangzheng2016/pvm/internal/cluster"
	"github.com/huangzheng2016/pvm/internal/config"
	"github.com/huangzheng2016/pvm/internal/image"
	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/qemu"
	"github.com/huangzheng2016/pvm/internal/vm"
)

type Server struct {
	P      *paths.Paths
	Cfg    *config.Config
	TC     *qemu.Toolchain
	Accel  qemu.Accel
	SSHPub string

	Clusters *cluster.Store
	relays   *relaySet

	started time.Time
	mu      sync.Mutex // guards vms cache
	vms     map[string]*vm.Spec

	agentMu sync.Mutex // guards agents
	agents  map[string]*agent.Client

	kasmMu sync.Mutex // guards kasms
	kasms  map[string]*kasmProxy
}

type vmInfo struct {
	Spec        *vm.Spec `json:"spec"`
	Running     bool     `json:"running"`
	PID         int      `json:"pid,omitempty"`
	Uptime      string   `json:"uptime,omitempty"`
	GatewaySSH  int      `json:"gw_ssh_port,omitempty"`
	GatewayUser string   `json:"gw_user,omitempty"`
}

func NewServer(p *paths.Paths, cfg *config.Config, tc *qemu.Toolchain, accel qemu.Accel, sshPub string, clusters *cluster.Store) *Server {
	return &Server{P: p, Cfg: cfg, TC: tc, Accel: accel, SSHPub: sshPub,
		Clusters: clusters, relays: newRelaySet(), started: time.Now(), vms: map[string]*vm.Spec{}, agents: map[string]*agent.Client{}}
}

// agentClient returns the shared per-VM agent connection, dialing if
// needed. QEMU's chardev socket accepts exactly one host connection,
// so every exec multiplexes over this single client. Fresh connections
// are Ping-checked: a half-open or protocol-desynced channel is
// discarded instead of handed to callers.
func (s *Server) agentClient(name string) (*agent.Client, error) {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	if c := s.agents[name]; c != nil && !c.Closed() {
		return c, nil
	}
	c, err := agent.DialSock(s.P.AgentSock(name))
	if err != nil {
		return nil, err
	}
	if _, err := c.Ping(); err != nil {
		c.Close()
		return nil, err
	}
	s.agents[name] = c
	return c, nil
}

// dropAgent discards a broken shared connection.
func (s *Server) dropAgent(name string, c *agent.Client) {
	s.agentMu.Lock()
	defer s.agentMu.Unlock()
	if s.agents[name] == c {
		delete(s.agents, name)
	}
	c.Close()
}

func (s *Server) instance(spec *vm.Spec) *qemu.Instance {
	var linkSock string
	poolBase, poolSize := 0, 0
	if spec.Role == "gateway" {
		if cl := s.Clusters.Get(spec.ClusterID); cl != nil {
			poolBase, poolSize = cl.PoolBase, cl.PoolSize
		} else if cl := s.clustersByGatewayName(spec.Name); cl != nil {
			poolBase, poolSize = cl.PoolBase, cl.PoolSize
		}
	} else if spec.ClusterID > 0 {
		linkSock = s.Clusters.LinkSock(spec.ClusterID, spec.Name)
	}
	cfg := spec.QEMUConfig(s.P, s.Accel, linkSock, poolBase, poolSize)
	vmDir := s.P.VMDir(spec.Name)
	cfg.DiskPath = qemu.ActiveDisk(vmDir, cfg.DiskPath)
	return &qemu.Instance{
		Cfg:       cfg,
		StatePath: filepath.Join(vmDir, "run.json"),
		Toolchain: s.TC,
	}
}

func (s *Server) clustersByGatewayName(name string) *cluster.Cluster {
	for _, cl := range s.Clusters.List() {
		if cl.Gateway == name {
			return cl
		}
	}
	return nil
}

func (s *Server) getSpec(name string) (*vm.Spec, error) {
	s.mu.Lock()
	cached := s.vms[name]
	s.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	spec, err := vm.Load(s.P, name)
	if err != nil {
		return nil, fmt.Errorf("vm %q not found", name)
	}
	s.mu.Lock()
	s.vms[name] = spec
	s.mu.Unlock()
	return spec, nil
}

func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.jsonOK(map[string]any{"ready": true}))
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("GET /v1/vms", s.handleList)
	mux.HandleFunc("POST /v1/vms", s.handleCreate)
	mux.HandleFunc("GET /v1/vms/{name}", s.handleGet)
	mux.HandleFunc("DELETE /v1/vms/{name}", s.handleDelete)
	mux.HandleFunc("POST /v1/vms/{name}/start", s.handleStart)
	mux.HandleFunc("POST /v1/vms/{name}/stop", s.handleStop)
	mux.HandleFunc("POST /v1/vms/{name}/snapshot", s.handleSnapshotCreate)
	mux.HandleFunc("GET /v1/vms/{name}/snapshot", s.handleSnapshotGet)
	mux.HandleFunc("DELETE /v1/vms/{name}/snapshot", s.handleSnapshotDelete)
	mux.HandleFunc("POST /v1/vms/{name}/exec", s.handleExec)
	mux.HandleFunc("GET /v1/clusters", s.handleClusters)
	mux.HandleFunc("GET /v1/maps", s.handleMapList)
	mux.HandleFunc("POST /v1/maps", s.handleMapAdd)
	mux.HandleFunc("DELETE /v1/maps/{port}/{proto}", s.handleMapRemove)
	mux.HandleFunc("GET /v1/routes", s.handleRouteList)
	mux.HandleFunc("POST /v1/routes", s.handleRouteAdd)
	mux.HandleFunc("DELETE /v1/routes/{index}", s.handleRouteRemove)
	mux.HandleFunc("POST /v1/vms/{name}/route", s.handleVMRoute)
	mux.HandleFunc("POST /v1/vms/{name}/kasm", s.handleKasm)
	mux.HandleFunc("GET /v1/dns", s.handleDNSList)
	mux.HandleFunc("POST /v1/shutdown", s.handleShutdown)
	return mux
}

func (s *Server) jsonOK(v any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, v) }
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	specs, _ := vm.ListAll(s.P)
	running := 0
	for _, sp := range specs {
		if s.instance(sp).Running() != nil {
			running++
		}
	}
	writeJSON(w, 200, map[string]any{
		"version": buildinfo.Version,
		"pid":     os.Getpid(),
		"uptime":  time.Since(s.started).Round(time.Second).String(),
		"vms":     len(specs),
		"running": running,
		"accel":   s.Accel,
		"qemu":    s.TC.System,
		"bundled": s.TC.Bundled,
	})
}

func (s *Server) collectInfos() []vmInfo {
	specs, _ := vm.ListAll(s.P)
	infos := make([]vmInfo, 0, len(specs))
	for _, sp := range specs {
		info := vmInfo{Spec: sp}
		if sp.Role != "gateway" && sp.ClusterID > 0 {
			if cl := s.Clusters.Get(sp.ClusterID); cl != nil {
				if gw, err := s.getSpec(cl.Gateway); err == nil {
					info.GatewaySSH = gw.SSHPort
					info.GatewayUser = gw.User
				}
			}
		}
		if st := s.instance(sp).Running(); st != nil {
			info.Running = true
			info.PID = st.PID
			info.Uptime = time.Since(st.Started).Round(time.Second).String()
		}
		infos = append(infos, info)
	}
	return infos
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.collectInfos())
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	spec, err := s.getSpec(r.PathValue("name"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	for _, info := range s.collectInfos() {
		if info.Spec.Name == spec.Name {
			writeJSON(w, 200, info)
			return
		}
	}
	writeJSON(w, 200, vmInfo{Spec: spec})
}

func (s *Server) handleClusters(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.Clusters.List())
}

type createRequest struct {
	Name   string   `json:"name"`
	Image  string   `json:"image"`
	User   string   `json:"user"`
	CPUs   int      `json:"cpus"`
	MemMB  int      `json:"mem_mb"`
	DiskGB int      `json:"disk_gb"`
	Mounts []string `json:"mounts"` // "/host:/guest"
	Start  bool     `json:"start"`
	// Region/identity profile (all optional).
	TZ          string `json:"tz,omitempty"`
	Locale      string `json:"locale,omitempty"`
	GitEmail    string `json:"git_email,omitempty"`
	GitName     string `json:"git_name,omitempty"`
	SmbiosSpoof bool   `json:"smbios_spoof,omitempty"`
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	lock := qemu.LockVM(req.Name)
	lock.Lock()
	defer lock.Unlock()

	if _, err := vm.Load(s.P, req.Name); err == nil {
		writeErr(w, 409, fmt.Errorf("vm %q already exists", req.Name))
		return
	}
	if req.CPUs == 0 {
		req.CPUs = s.Cfg.Defaults.CPUs
	}
	if req.MemMB == 0 {
		req.MemMB = s.Cfg.Defaults.MemMB
	}
	if req.DiskGB == 0 {
		req.DiskGB = s.Cfg.Defaults.DiskGB
	}
	if req.User == "" {
		req.User = "pvm"
	}

	mounts, err := parseMounts(req.Mounts)
	if err != nil {
		writeErr(w, 400, err)
		return
	}

	base := filepath.Join(s.P.Images, fmt.Sprintf("%s-%s.qcow2", imageFileStem(req.Image), runtime.GOARCH))
	goldenVer := 0
	if distro, version, ok := strings.Cut(req.Image, ":"); ok {
		if g := image.LatestGolden(s.P.Images, distro, version, runtime.GOARCH); g != nil {
			base = filepath.Join(image.GoldenDir(s.P.Images), g.File)
			goldenVer = g.Version
		}
	}
	if _, err := os.Stat(base); err != nil {
		writeErr(w, 400, fmt.Errorf("base image for %s not found; run `pvm image fetch %s`", req.Image, req.Image))
		return
	}

	cl, err := s.Clusters.Ensure(cluster.DefaultClusterID)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	vnet, err := s.Clusters.AddVM(cl.ID, req.Name)
	if err != nil {
		writeErr(w, 500, err)
		return
	}

	if req.TZ != "" {
		if err := vm.ValidateTZ(req.TZ); err != nil {
			writeErr(w, 400, err)
			return
		}
	}
	if req.Locale != "" {
		l, err := vm.NormalizeLocale(req.Locale)
		if err != nil {
			writeErr(w, 400, err)
			return
		}
		req.Locale = l
	}
	spec := &vm.Spec{
		Name: req.Name, Image: req.Image, Role: "user", User: req.User, Arch: runtime.GOARCH,
		GuestUID: os.Getuid(),
		CPUs:     req.CPUs, MemMB: req.MemMB, DiskGB: req.DiskGB,
		BaseDisk: base, Mounts: mounts, Golden: goldenVer,
		ClusterID: cl.ID, IP: vnet.IP, MAC: vnet.MAC,
		TZ: req.TZ, Locale: req.Locale,
		GitEmail: req.GitEmail, GitName: req.GitName,
		SmbiosSpoof: req.SmbiosSpoof,
	}
	if err := vm.Create(s.P, s.TC, spec, s.SSHPub); err != nil {
		s.Clusters.RemoveVM(cl.ID, req.Name)
		writeErr(w, 500, err)
		return
	}
	s.mu.Lock()
	s.vms[spec.Name] = spec
	s.mu.Unlock()

	if req.Start {
		if _, _, err := s.startVM(spec); err != nil {
			writeErr(w, 500, fmt.Errorf("created but start failed: %w", err))
			return
		}
		s.ensureAgentAsync(spec)
	}
	writeJSON(w, 201, vmInfo{Spec: spec, Running: req.Start})
}

// startVM ensures the cluster gateway and link are up before starting a
// cluster-attached VM, prepares a snapshot restore when one is pending,
// then starts QEMU. Starting a gateway schedules cluster link recovery
// for its VMs. The started instance and snapshot manifest (nil for a
// plain disk boot) are returned so callers can await the restore.
func (s *Server) startVM(spec *vm.Spec) (*qemu.Instance, *qemu.SnapshotManifest, error) {
	if spec.Role == "gateway" {
		inst := s.instance(spec)
		if err := inst.Start(); err != nil {
			return nil, nil, err
		}
		if cl := s.clustersByGatewayName(spec.Name); cl != nil {
			go s.onGatewayStarted(cl)
		}
		return inst, nil, nil
	}
	if spec.ClusterID > 0 {
		cl, err := s.ensureCluster(spec.ClusterID)
		if err != nil {
			return nil, nil, err
		}
		if err := s.ensureLink(cl, spec.Name); err != nil {
			return nil, nil, err
		}
		go s.deployClusterAgent(cl) // binary+config push is best-effort async
	}
	inst := s.instance(spec)
	man, err := inst.PrepareRestore(s.P.VMDir(spec.Name))
	if err != nil {
		return nil, nil, err
	}
	if err := inst.Start(); err != nil {
		return nil, nil, err
	}
	return inst, man, nil
}

func (s *Server) deployClusterAgent(cl *cluster.Cluster) {
	if err := s.deployGatewayAgent(cl); err != nil {
		log.Printf("deploy gateway agent: %v", err)
	}
}

func imageFileStem(imageSpec string) string {
	// "ubuntu:24.04" -> "ubuntu-24.04"
	out := make([]byte, 0, len(imageSpec))
	for _, c := range imageSpec {
		if c == ':' {
			out = append(out, '-')
		} else {
			out = append(out, byte(c))
		}
	}
	return string(out)
}

func parseMounts(in []string) ([]vm.GuestMount, error) {
	var out []vm.GuestMount
	for i, m := range in {
		host, guest, ok := cut2(m, ':')
		if !ok || host == "" || guest == "" {
			return nil, fmt.Errorf("invalid mount %q, want /host:/guest", m)
		}
		fi, err := os.Stat(host)
		if err != nil || !fi.IsDir() {
			return nil, fmt.Errorf("mount host path %s is not a directory", host)
		}
		out = append(out, vm.GuestMount{Tag: fmt.Sprintf("pvm%d", i), Host: host, Guest: guest})
	}
	return out, nil
}

func cut2(s string, sep byte) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == sep {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	spec, err := s.getSpec(r.PathValue("name"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	lock := qemu.LockVM(spec.Name)
	lock.Lock()
	defer lock.Unlock()
	inst, man, err := s.startVM(spec)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	s.ensureAgentAsync(spec)
	if man != nil {
		vmDir := s.P.VMDir(spec.Name)
		if err := inst.WaitRestored(vmDir); err != nil {
			inst.Stop()
			writeErr(w, 500, fmt.Errorf("restore failed, vm stopped: %w", err))
			return
		}
		// Shares were hot-unplugged before the snapshot and re-attached
		// after the restore; remount them (ssh needs a moment after
		// resume).
		go func() {
			for i := 0; i < 15; i++ {
				if err := s.sshGuest(spec, "sudo mount -a"); err == nil {
					return
				}
				time.Sleep(2 * time.Second)
			}
			log.Printf("restore %s: remount of 9p shares failed", spec.Name)
		}()
		writeJSON(w, 200, map[string]any{"started": spec.Name, "restored": man.CreatedAt})
		return
	}
	writeJSON(w, 200, map[string]any{"started": spec.Name})
}

// sshGuest runs a command inside the guest; used for snapshot-time 9p
// mount management (umount before migrate, remount after restore).
func (s *Server) sshGuest(spec *vm.Spec, remoteCmd string) error {
	_, err := s.sshRun(spec, remoteCmd)
	return err
}

func (s *Server) handleSnapshotCreate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	spec, err := s.getSpec(name)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	lock := qemu.LockVM(name)
	lock.Lock()
	defer lock.Unlock()

	// 9p mounts hold guest fid state the fresh QEMU process cannot
	// reconstruct; lazily unmount them before migration (best effort).
	mounts := []string{"/share"}
	for _, m := range spec.Mounts {
		mounts = append(mounts, m.Guest)
	}
	if err := s.sshGuest(spec, "sudo umount -l "+strings.Join(mounts, " ")); err != nil {
		log.Printf("snapshot %s: pre-migrate umount failed (continuing): %v", name, err)
	}

	if err := s.instance(spec).CreateSnapshot(s.P.VMDir(name), buildinfo.Version); err != nil {
		writeErr(w, 500, err)
		return
	}
	man, err := qemu.LoadManifest(s.P.VMDir(name))
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 201, man)
}

func (s *Server) handleSnapshotGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := s.getSpec(name); err != nil {
		writeErr(w, 404, err)
		return
	}
	man, err := qemu.LoadManifest(s.P.VMDir(name))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, 200, man)
}

func (s *Server) handleSnapshotDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	spec, err := s.getSpec(name)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	lock := qemu.LockVM(name)
	lock.Lock()
	defer lock.Unlock()
	if st := s.instance(spec).Running(); st != nil {
		writeErr(w, 409, fmt.Errorf("vm %s is running; stop it before removing the snapshot", name))
		return
	}
	if err := qemu.RemoveSnapshot(s.P.VMDir(name)); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"removed": name})
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	spec, err := s.getSpec(r.PathValue("name"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	lock := qemu.LockVM(spec.Name)
	lock.Lock()
	defer lock.Unlock()
	if err := s.instance(spec).Stop(); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"stopped": spec.Name})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	spec, err := s.getSpec(name)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	lock := qemu.LockVM(name)
	lock.Lock()
	defer lock.Unlock()
	if err := s.instance(spec).Stop(); err != nil {
		log.Printf("stop %s before delete: %v", name, err)
	}
	if spec.Role != "gateway" && spec.ClusterID > 0 {
		if cl := s.Clusters.Get(spec.ClusterID); cl != nil {
			s.removeLink(cl, name)
			s.Clusters.RemoveVM(cl.ID, name)
			s.pushGatewayConfig(cl)
		}
	}
	if err := vm.Delete(s.P, name); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.mu.Lock()
	delete(s.vms, name)
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"deleted": name})
}

var shutdownCh = make(chan struct{}, 1)

func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"bye": true})
	select {
	case shutdownCh <- struct{}{}:
	default:
	}
}

// StopAllVMs gracefully stops every running VM (used on SIGTERM).
func (s *Server) StopAllVMs() {
	for _, info := range s.collectInfos() {
		if !info.Running {
			continue
		}
		lock := qemu.LockVM(info.Spec.Name)
		lock.Lock()
		if err := s.instance(info.Spec).Stop(); err != nil {
			log.Printf("graceful stop %s: %v", info.Spec.Name, err)
		}
		lock.Unlock()
	}
}

// recoverClusters restores gateways, links, port-map relays and agent
// configs after a daemon (re)start.
func (s *Server) recoverClusters() {
	for _, cl := range s.Clusters.List() {
		if _, err := s.ensureCluster(cl.ID); err != nil {
			log.Printf("recover cluster %d: %v", cl.ID, err)
			continue
		}
		for _, vn := range cl.VMs {
			spec, err := s.getSpec(vn.Name)
			if err != nil {
				continue
			}
			if s.instance(spec).Running() != nil {
				if err := s.ensureLink(cl, vn.Name); err != nil {
					log.Printf("recover link %s: %v", vn.Name, err)
				}
			}
		}
		for _, m := range cl.Maps {
			if err := s.relays.Ensure(m.Proto, m.HostPort, m.PoolPort); err != nil {
				log.Printf("recover relay %s/%d: %v", m.Proto, m.HostPort, err)
			}
		}
		go s.deployClusterAgent(cl)
	}
}

// Run starts the unix-socket HTTP server and blocks until shutdown.
func Run(p *paths.Paths, srv *Server) error {
	sock := p.DaemonSock()
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	os.Chmod(sock, 0o600)

	httpSrv := &http.Server{Handler: srv.handler()}
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.Serve(ln) }()
	go srv.recoverClusters()

	select {
	case <-sigCh:
	case <-shutdownCh:
	case err := <-errCh:
		return err
	}

	log.Println("daemon shutting down: stopping VMs")
	srv.StopAllVMs()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	httpSrv.Shutdown(ctx)
	ln.Close()
	os.Remove(sock)
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}
