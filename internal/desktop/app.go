// Package desktop is the backend of PVM Desktop. It reads state from the
// daemon API and the store packages, and changes state only by running
// the pvm CLI as a child process, so the app and the CLI share one
// implementation of every operation.
package desktop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/huangzheng2016/pvm/internal/buildinfo"
	"github.com/huangzheng2016/pvm/internal/cli"
	"github.com/huangzheng2016/pvm/internal/daemon"
	"github.com/huangzheng2016/pvm/internal/paths"
)

const (
	readTimeout   = 10 * time.Second
	followTimeout = 20 * time.Minute // the CLI's own provisioning limit
	pollInterval  = 2 * time.Second
	// lostTimeout is how long a wait goes on while the daemon cannot be
	// read: long enough for a daemon restart.
	lostTimeout = 30 * time.Second
)

// daemonReader is the read side of daemon.Client.
type daemonReader interface {
	Healthy() bool
	Get(path string, out any) error
}

// clientReader bounds every read: daemon.Client alone waits 20 minutes.
type clientReader struct{ c *daemon.Client }

func (r clientReader) Healthy() bool { return r.c.Healthy() }

func (r clientReader) Get(path string, out any) error {
	ctx, cancel := context.WithTimeout(context.Background(), readTimeout)
	defer cancel()
	return r.c.GetContext(ctx, path, out)
}

// Options are the parts of the window toolkit the backend needs.
type Options struct {
	// Emit sends an event to the frontend.
	Emit func(event string, data any)
	// PickFolder shows a folder dialog and returns "" when cancelled.
	PickFolder func() (string, error)
}

// App is bound to the frontend as window.go.desktop.App. Every exported
// method is callable from the page, so helpers stay unexported.
type App struct {
	p       *paths.Paths
	daemon  daemonReader
	run     runner
	sys     system
	emit    func(event string, data any)
	pick    func() (string, error)
	backend func() (string, error)

	followTimeout time.Duration
	pollInterval  time.Duration
	lostTimeout   time.Duration

	backendMu   sync.Mutex
	backendName string
	taskID      atomic.Int64

	runningMu sync.Mutex
	running   map[int64]string // the titles of the tasks that have not ended
}

func New(p *paths.Paths, o Options) *App {
	return &App{
		p:             p,
		daemon:        clientReader{daemon.NewClient(p)},
		run:           pvmRunner{find: pvmPath},
		sys:           hostSystem(),
		emit:          o.Emit,
		pick:          o.PickFolder,
		backend:       func() (string, error) { return cli.ResolveWindowsBackend(p) },
		followTimeout: followTimeout,
		pollInterval:  pollInterval,
		lostTimeout:   lostTimeout,
		running:       map[int64]string{},
	}
}

// RunningTasks returns the titles of the actions that have not ended,
// for the window to ask before it closes. It is a function because every
// exported method of App is callable from the page.
func RunningTasks(a *App) []string {
	a.runningMu.Lock()
	defer a.runningMu.Unlock()
	titles := make([]string, 0, len(a.running))
	for _, title := range a.running {
		titles = append(titles, title)
	}
	slices.Sort(titles)
	return titles
}

const tokenFilePattern = "exit-token-*"

// RemoveTokenFiles removes the token files of AddExit that an app which
// was killed or closed during `pvm exit add` left in the run directory.
// The CLI reads the file as it starts, so no running command needs one.
func RemoveTokenFiles(p *paths.Paths) error {
	left, err := filepath.Glob(filepath.Join(p.Run, tokenFilePattern))
	if err != nil {
		return err
	}
	var errs []error
	for _, file := range left {
		errs = append(errs, os.Remove(file))
	}
	return errors.Join(errs...)
}

// resolveBackend caches the answer: in auto mode it probes the daemon,
// QEMU and WSL, which is too slow for every poll.
func (a *App) resolveBackend() (string, error) {
	a.backendMu.Lock()
	defer a.backendMu.Unlock()
	if a.backendName == "" {
		name, err := a.backend()
		if err != nil {
			return "", err
		}
		a.backendName = name
	}
	return a.backendName, nil
}

var errWSL = errors.New("PVM Desktop works with the native Windows backend only; with the WSL2 backend use the pvm command line")

// native refuses every operation under the WSL2 backend, where the
// daemon and its state live inside WSL.
func (a *App) native() error {
	name, err := a.resolveBackend()
	if err != nil {
		return err
	}
	if name != "native" {
		return errWSL
	}
	return nil
}

// State is what the app shows before anything else and in About.
type State struct {
	Backend       string `json:"backend"` // native or wsl
	DaemonHealthy bool   `json:"daemon_healthy"`
	// DaemonStopped is true when the user stopped the daemon on purpose;
	// the app then offers to start it instead of starting it.
	DaemonStopped bool   `json:"daemon_stopped"`
	AppVersion    string `json:"app_version"`
	DaemonVersion string `json:"daemon_version"`
	Accel         string `json:"accel"`
	// TCGFallback counts running VMs that fell back to software emulation.
	TCGFallback int    `json:"tcg_fallback"`
	DataDir     string `json:"data_dir"`
}

// State works under every backend; it is how the page learns that the
// backend is WSL2.
func (a *App) State() (State, error) {
	backend, err := a.resolveBackend()
	if err != nil {
		return State{}, err
	}
	st := State{Backend: backend, AppVersion: buildinfo.Version, DataDir: a.p.Home}
	if backend != "native" {
		return st, nil
	}
	if _, err := os.Stat(a.p.DaemonStopMark()); err == nil {
		st.DaemonStopped = true
	}
	if !a.daemon.Healthy() {
		return st, nil
	}
	st.DaemonHealthy = true
	var status struct {
		Version     string `json:"version"`
		Accel       string `json:"accel"`
		TCGFallback int    `json:"tcg_fallback"`
	}
	if err := a.daemon.Get("/v1/status", &status); err != nil {
		return State{}, fmt.Errorf("daemon status: %w", err)
	}
	st.DaemonVersion, st.Accel, st.TCGFallback = status.Version, status.Accel, status.TCGFallback
	return st, nil
}
