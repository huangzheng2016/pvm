package daemon

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/huangzheng2016/pvm/internal/agent"
	"github.com/huangzheng2016/pvm/internal/apps"
	"github.com/huangzheng2016/pvm/internal/vm"
)

// appInstallTimeout bounds one catalog install script. With the HTTP
// client's 20 minute limit on a whole response, a client sends one id per
// request and prepares a window app through apps/prepare first.
var appInstallTimeout = 15 * time.Minute

// catalogApp looks up a catalog app; tests replace it.
var catalogApp = apps.Get

// catalogAppFor returns the catalog app that prog starts.
func catalogAppFor(prog string) (string, bool) {
	for _, a := range apps.List() {
		if f := strings.Fields(a.Start); len(f) > 0 && f[0] == path.Base(prog) {
			return a.ID, true
		}
	}
	return "", false
}

func (s *Server) handleAppCatalog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, apps.List())
}

// appInstallFrame is one line of the install stream. App names the app a
// line of output or a state change belongs to. The last frame has Done,
// the apps this call installed, and Error when one failed.
type appInstallFrame struct {
	App       string   `json:"app,omitempty"`
	Out       string   `json:"out,omitempty"`
	State     string   `json:"state,omitempty"` // installing or installed
	Done      bool     `json:"done,omitempty"`
	Installed []string `json:"installed,omitempty"`
	Error     string   `json:"error,omitempty"`
}

// handleAppInstall installs catalog apps in the VM one after another and
// records each in the VM's app state as it succeeds. The first failure
// ends the run; apps installed before it stay recorded.
func (s *Server) handleAppInstall(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, fmt.Errorf("bad request: %v", err))
		return
	}
	if len(req.IDs) == 0 {
		writeErr(w, 400, fmt.Errorf("give at least one app id"))
		return
	}
	var list []apps.App
	for _, id := range req.IDs {
		a, ok := catalogApp(id)
		if !ok {
			writeErr(w, 400, fmt.Errorf("unknown app %q; see `pvm app catalog`", id))
			return
		}
		if !slices.ContainsFunc(list, func(b apps.App) bool { return b.ID == id }) {
			list = append(list, a)
		}
	}
	spec, ok := s.appVM(w, r.PathValue("name"))
	if !ok {
		return
	}
	v, _ := s.apps.installs.LoadOrStore(spec.Name, &sync.Mutex{})
	installs := v.(*sync.Mutex)
	installs.Lock()
	defer installs.Unlock()
	ac, err := s.agentClientContext(r.Context(), spec.Name)
	if err != nil {
		writeErr(w, 502, fmt.Errorf("agent of %s: %w", spec.Name, err))
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)
	var mu sync.Mutex
	send := func(f appInstallFrame) {
		mu.Lock()
		defer mu.Unlock()
		if enc.Encode(f) == nil {
			rc.Flush()
		}
	}
	var installed []string
	for i, a := range list {
		send(appInstallFrame{App: a.ID, State: "installing"})
		err := s.installCatalogApp(r.Context(), spec, ac, a, func(l string) { send(appInstallFrame{App: a.ID, Out: l}) })
		if err != nil {
			msg := fmt.Sprintf("install %s in %s: %v", a.ID, spec.Name, err)
			if rest := list[i+1:]; len(rest) > 0 {
				var ids []string
				for _, b := range rest {
					ids = append(ids, b.ID)
				}
				msg += "; not attempted: " + strings.Join(ids, ", ")
			}
			send(appInstallFrame{Done: true, Installed: installed, Error: msg})
			return
		}
		installed = append(installed, a.ID)
		send(appInstallFrame{App: a.ID, State: "installed"})
	}
	send(appInstallFrame{Done: true, Installed: installed})
}

// installCatalogApp runs a's script as root through the guest agent and
// records the app. A window app gets Xpra first. The VM's app lock is
// held for the Xpra step and the record only, not while the script runs.
func (s *Server) installCatalogApp(ctx context.Context, spec *vm.Spec, ac *agent.Client, a apps.App, out func(string)) error {
	lock := s.appLock(spec.Name)
	if a.Window {
		pctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
		lock.Lock()
		_, err := s.prepareAppSupport(pctx, spec, out)
		lock.Unlock()
		cancel()
		if err != nil {
			return err
		}
	}
	sctx, cancel := context.WithTimeout(ctx, appInstallTimeout)
	defer cancel()
	stdout, stderr := lineWriter(out), lineWriter(out)
	// The guest's own limit only backs up the daemon's, which reports the
	// timeout. spec.User is a validated name and travels as one argument.
	res, err := ac.ExecStreamContext(sctx, []string{"env", "PVM_USER=" + spec.User, "bash", "-s"}, strings.NewReader(a.Script), int(appInstallTimeout/time.Second)+60, func(f *agent.Frame) {
		data, derr := base64.StdEncoding.DecodeString(f.DataB64)
		if derr != nil {
			return
		}
		switch f.Stream {
		case agent.StreamStdout:
			stdout.Write(data)
		case agent.StreamStderr:
			stderr.Write(data)
		}
	})
	stdout.Close()
	stderr.Close()
	switch {
	case err != nil && ctx.Err() == nil && errors.Is(sctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("timed out after %s", appInstallTimeout)
	case err != nil:
		// Once dispatched, the script may have run even when no frame
		// arrives. Never replay it after a transport failure.
		return fmt.Errorf("result unknown; the script was not run again: %w", err)
	case res.ExitCode != 0:
		return fmt.Errorf("the install script failed with exit status %d", res.ExitCode)
	}
	lock.Lock()
	defer lock.Unlock()
	st, err := s.loadAppState(spec.Name)
	if err != nil {
		return err
	}
	if slices.Contains(st.Installed, a.ID) {
		return nil
	}
	st.Installed = append(st.Installed, a.ID)
	slices.Sort(st.Installed)
	return s.saveAppState(spec.Name, st)
}
