package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/huangzheng2016/pvm/internal/fsutil"
	"github.com/huangzheng2016/pvm/internal/workspace"
	"github.com/huangzheng2016/pvm/internal/workspace/sftpd"
)

// Workspace mount states reported by /v1/workspaces/{id}/mount.
const (
	mountMounted    = "mounted"
	mountConnecting = "connecting"
	mountError      = "error"
	mountStopped    = "stopped"
	mountAbsent     = "absent"
)

type mountStatus struct {
	State string `json:"state"`
	Error string `json:"error,omitempty"`
	Since string `json:"since,omitempty"`
}

// mountSupervisor keeps one reverse-sshfs session per sftp mount
// workspace whose VM is up, and restarts it with backoff when it drops.
type mountSupervisor struct {
	list    func() ([]*workspace.Workspace, error)
	vmUp    func(name string) bool
	session func(ctx context.Context, ws *workspace.Workspace, mounted func()) error

	backoffMin, backoffMax time.Duration

	reconcileMu sync.Mutex
	mu          sync.Mutex
	ctx         context.Context
	workers     map[string]*mountWorker
	status      map[string]mountStatus
	held        map[string]bool // stopped by DELETE until the next POST
	wg          sync.WaitGroup
}

type mountWorker struct {
	ws     workspace.Workspace
	cancel context.CancelFunc
	done   chan struct{}
}

// sftpMount reports whether the workspace is served by this supervisor.
func sftpMount(ws *workspace.Workspace) bool {
	return ws.Mode == workspace.ModeMount && ws.Transport == workspace.TransportSFTP
}

func sameMount(a, b *workspace.Workspace) bool {
	return a.VM == b.VM && a.HostPath == b.HostPath && a.GuestPath == b.GuestPath && a.ReadOnly == b.ReadOnly
}

// run reconciles every interval until ctx is done, then stops all workers.
func (m *mountSupervisor) run(ctx context.Context, interval time.Duration) {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		m.reconcile()
		select {
		case <-ctx.Done():
			m.wg.Wait()
			return
		case <-t.C:
		}
	}
}

func (m *mountSupervisor) reconcile() {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()
	list, err := m.list()
	if err != nil {
		// Keep running sessions; a listing error says nothing about them.
		log.Printf("workspace mounts: %v", err)
		return
	}
	want := map[string]*workspace.Workspace{}
	up := map[string]bool{}
	for _, ws := range list {
		if !sftpMount(ws) {
			continue
		}
		want[ws.ID] = ws
		if _, ok := up[ws.VM]; !ok {
			up[ws.VM] = m.vmUp(ws.VM)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.initLocked()
	for id, w := range m.workers {
		if ws := want[id]; ws == nil || m.held[id] || !up[ws.VM] || !sameMount(ws, &w.ws) {
			m.stopLocked(id)
		}
	}
	for id := range m.status {
		if want[id] == nil {
			delete(m.status, id)
			delete(m.held, id)
		}
	}
	running := m.ctx != nil && m.ctx.Err() == nil
	for id, ws := range want {
		if m.workers[id] != nil {
			continue
		}
		if m.held[id] || !up[ws.VM] || !running {
			m.setLocked(id, mountStopped, "")
			continue
		}
		m.startLocked(ws)
	}
}

func (m *mountSupervisor) initLocked() {
	if m.status == nil {
		m.workers, m.status, m.held = map[string]*mountWorker{}, map[string]mountStatus{}, map[string]bool{}
	}
}

func (m *mountSupervisor) setLocked(id, state, msg string) {
	st := m.status[id]
	if st.State != state || st.Since == "" {
		st.Since = time.Now().UTC().Format(time.RFC3339)
	}
	st.State, st.Error = state, msg
	m.status[id] = st
}

// set ignores updates from a worker that has been replaced or stopped.
func (m *mountSupervisor) set(id string, w *mountWorker, state, msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.workers[id] == w {
		m.setLocked(id, state, msg)
	}
}

func (m *mountSupervisor) startLocked(ws *workspace.Workspace) {
	ctx, cancel := context.WithCancel(m.ctx)
	w := &mountWorker{ws: *ws, cancel: cancel, done: make(chan struct{})}
	m.workers[ws.ID] = w
	m.setLocked(ws.ID, mountConnecting, "")
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(w.done)
		m.work(ctx, w)
	}()
}

// stopLocked cancels the worker and returns a channel closed once it
// has exited, or nil when there was none.
func (m *mountSupervisor) stopLocked(id string) <-chan struct{} {
	w := m.workers[id]
	if w == nil {
		return nil
	}
	delete(m.workers, id)
	w.cancel()
	m.setLocked(id, mountStopped, "")
	return w.done
}

func (m *mountSupervisor) work(ctx context.Context, w *mountWorker) {
	id := w.ws.ID
	backoff := m.backoffMin
	for {
		m.set(id, w, mountConnecting, "")
		mounted := false
		err := m.session(ctx, &w.ws, func() {
			mounted = true
			m.set(id, w, mountMounted, "")
			log.Printf("workspace %s: mounted at %s:%s", id, w.ws.VM, w.ws.GuestPath)
		})
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("sshfs session ended")
		}
		if mounted {
			backoff = m.backoffMin
		}
		m.set(id, w, mountError, err.Error())
		log.Printf("workspace %s: mount: %v; retrying in %s", id, err, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, m.backoffMax)
	}
}

func (m *mountSupervisor) get(id string) mountStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st, ok := m.status[id]; ok {
		return st
	}
	return mountStatus{State: mountAbsent}
}

// start lifts a DELETE hold and reconciles at once.
func (m *mountSupervisor) start(id string) mountStatus {
	m.mu.Lock()
	delete(m.held, id)
	m.mu.Unlock()
	m.reconcile()
	return m.get(id)
}

// stop holds the workspace stopped and waits for its session to exit.
func (m *mountSupervisor) stop(ctx context.Context, id string) error {
	m.reconcileMu.Lock()
	m.mu.Lock()
	m.initLocked()
	m.held[id] = true
	done := m.stopLocked(id)
	m.mu.Unlock()
	m.reconcileMu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("workspace %s: mount session did not stop: %w", id, ctx.Err())
	}
}

// workspaceMounts wires the supervisor to the daemon on first use.
func (s *Server) workspaceMounts() *mountSupervisor {
	s.wsMountOnce.Do(func() {
		s.wsMount = &mountSupervisor{
			list:       workspace.NewStore(s.P).List,
			vmUp:       s.workspaceVMUp,
			session:    s.workspaceMountSession,
			backoffMin: 2 * time.Second,
			backoffMax: time.Minute,
		}
	})
	return s.wsMount
}

// workspaceVMUp reports whether a user VM is running and past boot.
// A running VM with no recorded stages was started by an earlier daemon.
func (s *Server) workspaceVMUp(name string) bool {
	spec, err := s.getSpec(name)
	if err != nil || spec.Role == "gateway" || spec.IdentityReset {
		return false
	}
	if st, err := s.instance(spec).Running(); err != nil || st == nil {
		return false
	}
	return s.vmReady(name) || len(s.stages(name)) == 0
}

// workspaceMountSession runs one sshfs session: the guest's sshfs speaks
// SFTP over the ssh session's stdout and stdin, and pvm answers from the
// host folder. It returns when the session ends.
func (s *Server) workspaceMountSession(ctx context.Context, ws *workspace.Workspace, mounted func()) error {
	spec, err := s.getSpec(ws.VM)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(ws.HostPath); err != nil {
		return err
	} else if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", ws.HostPath)
	}
	mountCmd, err := sftpd.MountCommand(ws.GuestPath, ws.ReadOnly)
	if err != nil {
		return err
	}
	mountedCmd, err := sftpd.MountedCommand(ws.GuestPath)
	if err != nil {
		return err
	}
	ictx, icancel := context.WithTimeout(ctx, 10*time.Minute)
	_, err = s.sshRunCtx(ictx, spec, sftpd.InstallCommand())
	icancel()
	if err != nil {
		return fmt.Errorf("install sshfs in %s: %w", ws.VM, err)
	}
	args, target, err := s.sshBaseArgs(spec)
	if err != nil {
		return err
	}
	args = append(args, "-T", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", target, mountCmd)

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c := exec.CommandContext(cctx, "ssh", args...)
	fsutil.ConfigureHiddenCmd(c)
	c.WaitDelay = 2 * time.Second
	stdin, err := c.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	stderr := &tailWriter{max: 4096}
	c.Stderr = stderr
	if err := c.Start(); err != nil {
		return err
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- sftpd.Serve(cctx, stdout, stdin, ws.HostPath, ws.ReadOnly) }()
	confirmed := make(chan struct{})
	go func() {
		defer close(confirmed)
		// sshfs answers only after its SFTP handshake; the mount table is
		// the proof the guest sees the folder.
		deadline := time.Now().Add(2 * time.Minute)
		for cctx.Err() == nil {
			pctx, pcancel := context.WithTimeout(cctx, 15*time.Second)
			_, err := s.sshRunCtx(pctx, spec, mountedCmd)
			pcancel()
			if err == nil {
				mounted()
				return
			}
			if time.Now().After(deadline) {
				stderr.Write([]byte("mount did not appear in the guest within 2m\n"))
				cancel()
				return
			}
			select {
			case <-cctx.Done():
			case <-time.After(time.Second):
			}
		}
	}()
	sErr := <-serveErr
	// stdout reached EOF, so ssh is exiting; Wait must come after the
	// last read, and cancel covers an ssh that lingers.
	cancel()
	wErr := c.Wait()
	<-confirmed
	if ctx.Err() != nil {
		return ctx.Err()
	}
	err = errors.Join(sErr, wErr)
	if msg := stderr.lastLines(3); msg != "" {
		return fmt.Errorf("%v: %s", err, msg)
	}
	return err
}

// tailWriter keeps the last max bytes written to it.
type tailWriter struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = t.b[len(t.b)-t.max:]
	}
	return len(p), nil
}

func (t *tailWriter) lastLines(n int) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(string(t.b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func (s *Server) handleWorkspaceMountGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := workspace.ValidID(id); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, s.workspaceMounts().get(id))
}

func (s *Server) handleWorkspaceMountStart(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := workspace.ValidID(id); err != nil {
		writeErr(w, 400, err)
		return
	}
	if _, err := workspace.NewStore(s.P).Load(id); errors.Is(err, workspace.ErrNotFound) {
		writeErr(w, 404, err)
		return
	} else if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, s.workspaceMounts().start(id))
}

func (s *Server) handleWorkspaceMountStop(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := workspace.ValidID(id); err != nil {
		writeErr(w, 400, err)
		return
	}
	m := s.workspaceMounts()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := m.stop(ctx, id); err != nil {
		writeErr(w, 500, err)
		return
	}
	ws, err := workspace.NewStore(s.P).Load(id)
	if errors.Is(err, workspace.ErrNotFound) {
		writeJSON(w, 200, m.get(id))
		return
	} else if err != nil {
		writeErr(w, 500, err)
		return
	}
	if sftpMount(ws) && s.workspaceVMUp(ws.VM) {
		cmd, err := sftpd.UnmountCommand(ws.GuestPath)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		spec, err := s.getSpec(ws.VM)
		if err == nil {
			_, err = s.sshRunCtx(ctx, spec, cmd)
		}
		if err != nil {
			writeErr(w, 500, fmt.Errorf("unmount %s in %s: %w", ws.GuestPath, ws.VM, err))
			return
		}
	}
	writeJSON(w, 200, m.get(id))
}
