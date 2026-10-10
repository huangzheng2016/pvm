// Package daemon is the always-resident control plane: HTTP/JSON over a
// unix socket, owning all VM lifecycle. CLI commands are thin clients.
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

	"github.com/huangzheng2016/pvm/internal/buildinfo"
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

	started time.Time
	mu      sync.Mutex // guards vms cache
	vms     map[string]*vm.Spec
}

type vmInfo struct {
	Spec    *vm.Spec `json:"spec"`
	Running bool     `json:"running"`
	PID     int      `json:"pid,omitempty"`
	Uptime  string   `json:"uptime,omitempty"`
}

func NewServer(p *paths.Paths, cfg *config.Config, tc *qemu.Toolchain, accel qemu.Accel, sshPub string) *Server {
	return &Server{P: p, Cfg: cfg, TC: tc, Accel: accel, SSHPub: sshPub, started: time.Now(), vms: map[string]*vm.Spec{}}
}

func (s *Server) instance(spec *vm.Spec) *qemu.Instance {
	return &qemu.Instance{
		Cfg:       spec.QEMUConfig(s.P, s.Accel),
		StatePath: filepath.Join(s.P.VMDir(spec.Name), "run.json"),
		Toolchain: s.TC,
	}
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
		"version":  buildinfo.Version,
		"pid":      os.Getpid(),
		"uptime":   time.Since(s.started).Round(time.Second).String(),
		"vms":      len(specs),
		"running":  running,
		"accel":    s.Accel,
		"qemu":     s.TC.System,
		"bundled":  s.TC.Bundled,
	})
}

func (s *Server) collectInfos() []vmInfo {
	specs, _ := vm.ListAll(s.P)
	infos := make([]vmInfo, 0, len(specs))
	for _, sp := range specs {
		info := vmInfo{Spec: sp}
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

type createRequest struct {
	Name   string   `json:"name"`
	Image  string   `json:"image"`
	User   string   `json:"user"`
	CPUs   int      `json:"cpus"`
	MemMB  int      `json:"mem_mb"`
	DiskGB int      `json:"disk_gb"`
	Mounts []string `json:"mounts"` // "/host:/guest"
	Start  bool     `json:"start"`
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

	port, err := vm.AllocSSHPort(vm.UsedSSHPorts(s.P), vm.SSHPortBase)
	if err != nil {
		writeErr(w, 500, err)
		return
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
	spec := &vm.Spec{
		Name: req.Name, Image: req.Image, User: req.User, Arch: runtime.GOARCH,
		GuestUID: os.Getuid(),
		CPUs:     req.CPUs, MemMB: req.MemMB, DiskGB: req.DiskGB,
		SSHPort: port, BaseDisk: base, Mounts: mounts, Golden: goldenVer,
	}
	if err := vm.Create(s.P, s.TC, spec, s.SSHPub); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.mu.Lock()
	s.vms[spec.Name] = spec
	s.mu.Unlock()

	if req.Start {
		if err := s.instance(spec).Start(); err != nil {
			writeErr(w, 500, fmt.Errorf("created but start failed: %w", err))
			return
		}
	}
	writeJSON(w, 201, vmInfo{Spec: spec, Running: req.Start})
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
	if err := s.instance(spec).Start(); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"started": spec.Name})
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
