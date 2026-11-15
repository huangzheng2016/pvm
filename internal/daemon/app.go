package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/huangzheng2016/pvm/internal/fsutil"
	"github.com/huangzheng2016/pvm/internal/image"
	"github.com/huangzheng2016/pvm/internal/vm"
)

// `pvm app` shows single guest applications as host windows through Xpra.
// Each VM runs one Xpra server on appDisplay as the VM user, and every app
// is a child of it, so apps in a VM share one X display and clipboard.
// The server and each app run as user systemd units, which stop the whole
// process tree. The host reaches the server's unix socket over pvm.stream.
const (
	appDisplay    = ":100"
	appServerUnit = "pvm-xpra"
	appUnitPrefix = "pvm-app-"
)

var appIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// appDropLimit caps one file dropped into a VM.
const appDropLimit = "100M"

type appRecord struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"` // "cmd" or "url"
	Argv    []string  `json:"argv,omitempty"`
	URL     string    `json:"url,omitempty"`
	Kiosk   bool      `json:"kiosk,omitempty"`
	Created time.Time `json:"created"`
}

// appState is persisted under the VM dir. Port keeps the host listener
// stable across daemon restarts, so an attached client reconnects.
type appState struct {
	Port int         `json:"port,omitempty"`
	Apps []appRecord `json:"apps"`
	// Clipboard is off (the default), to-guest, to-host or both.
	Clipboard string `json:"clipboard,omitempty"`
	// Drop lets files dropped on the VM's windows upload into it.
	Drop bool `json:"drop,omitempty"`
}

var clipboardModes = []string{"off", "to-guest", "to-host", "both"}

func (st *appState) clipboard() string {
	if st.Clipboard == "" {
		return "off"
	}
	return st.Clipboard
}

// clipToGuest reports whether host clipboard content may enter the VM.
func (st *appState) clipToGuest() bool {
	return st.Clipboard == "to-guest" || st.Clipboard == "both"
}

type appView struct {
	VM string `json:"vm"`
	appRecord
	State string `json:"state"` // running, exited or unknown
}

// appAttach tells the CLI how to connect the host client.
type appAttach struct {
	Port         int    `json:"port"`
	PasswordFile string `json:"password_file"`
	Title        string `json:"title"`
	Clipboard    string `json:"clipboard"`
	Drop         bool   `json:"drop"`
	// Connected is true while a client is attached or one was started in
	// the last appAttachGrace; the CLI then starts no second client.
	Connected bool `json:"connected"`
}

type appStartRequest struct {
	ID    string   `json:"id"`
	Argv  []string `json:"argv"`
	URL   string   `json:"url"`
	Kiosk bool     `json:"kiosk"`
}

type appStartResponse struct {
	App    appView   `json:"app"`
	Attach appAttach `json:"attach"`
}

const appAttachGrace = 20 * time.Second

type appHost struct {
	mu      sync.Mutex
	fwd     map[string]*streamForward
	pending map[string]time.Time // last client start per VM
	locks   sync.Map             // vm name -> *sync.Mutex
}

func (s *Server) appLock(name string) *sync.Mutex {
	v, _ := s.apps.locks.LoadOrStore(name, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func (s *Server) appDir(name string) string { return filepath.Join(s.P.VMDir(name), "app") }

func (s *Server) loadAppState(name string) (*appState, error) {
	raw, err := os.ReadFile(filepath.Join(s.appDir(name), "app.json"))
	if errors.Is(err, os.ErrNotExist) {
		return &appState{}, nil
	}
	if err != nil {
		return nil, err
	}
	var st appState
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("app state of %s: %w", name, err)
	}
	return &st, nil
}

func (s *Server) saveAppState(name string, st *appState) error {
	if err := os.MkdirAll(s.appDir(name), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(filepath.Join(s.appDir(name), "app.json"), raw, 0o600)
}

// appSecret returns the host copy of the VM's Xpra password, creating it.
func (s *Server) appSecret(name string) (string, string, error) {
	p := filepath.Join(s.appDir(name), "xpra.secret")
	if raw, err := os.ReadFile(p); err == nil && len(strings.TrimSpace(string(raw))) == 64 {
		return p, strings.TrimSpace(string(raw)), nil
	}
	if err := os.MkdirAll(s.appDir(name), 0o700); err != nil {
		return "", "", err
	}
	secret := randHex(32)
	if err := fsutil.AtomicWrite(p, []byte(secret), 0o600); err != nil {
		return "", "", err
	}
	return p, secret, nil
}

// appInstallScript installs the Xpra server unless it is there. It runs
// as the VM user, who has passwordless sudo.
func appInstallScript() string {
	const apt = "sudo -n env DEBIAN_FRONTEND=noninteractive apt-get -q -o DPkg::Lock::Timeout=300"
	// the mask also runs where xpra came preinstalled
	return "{ { command -v xpra && command -v xclip; } >/dev/null 2>&1 || { sudo -n bash -euo pipefail -c " + shellQuote(image.XpraRepoScript) +
		" && " + apt + " update && " + apt + " install -y --no-install-recommends " + strings.Join(image.XpraPackages, " ") + "; }; }" +
		" && { [ \"$(systemctl is-enabled xpra-server.socket 2>/dev/null)\" = masked ] || ! systemctl cat xpra-server.socket >/dev/null 2>&1 || sudo -n sh -c " + shellQuote(image.XpraMaskScript) + "; }"
}

// appServerScript starts the Xpra server unit unless it runs, and waits
// for its display. The password arrives on stdin. Linger keeps the user
// manager, and with it the server, alive after the ssh session ends.
func appServerScript() string {
	opts := []string{
		"--daemon=no", "--mdns=no", "--pulseaudio=no", "--speaker=off", "--microphone=off",
		"--webcam=no", "--printing=no", "--notifications=no", "--html=off", "--http-scripts=off",
		// the client decides whether files flow; uploads are never opened
		"--file-transfer=yes", "--open-files=no", "--open-url=no", "--file-size-limit=" + appDropLimit,
		// guest to host only; host content enters the guest through pvm's
		// focus-gated bridge, and the client decides whether it accepts
		"--clipboard=yes", "--clipboard-direction=to-client",
		"--systemd-run=no", "--dbus-launch=no", "--exit-with-children=no",
		// latency first: no video encoder (the guest has no AVX under
		// WHPX), fast lossless or near-lossless picture encodings
		"--speed=100", "--min-speed=80", "--quality=80", "--min-quality=50",
		"--encodings=rgb,webp,jpeg,png", "--video-encoders=none",
	}
	return `set -e
loginctl show-user "$USER" -p Linger 2>/dev/null | grep -qx Linger=yes || sudo -n loginctl enable-linger "$USER"
d="${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/pvm"
mkdir -p "$d"
chmod 700 "$d"
(umask 077; cat > "$d/xpra.secret")
if ! systemctl --user is-active --quiet ` + appServerUnit + `; then
  systemctl --user reset-failed ` + appServerUnit + ` >/dev/null 2>&1 || true
  rm -f "$d/xpra.sock"
  systemd-run --user --quiet --collect --unit=` + appServerUnit + ` -- xpra start ` + appDisplay + ` --bind="$d/xpra.sock,auth=file:filename=$d/xpra.secret" ` + strings.Join(opts, " ") + `
fi
for i in $(seq 1 60); do
  if [ -S "$d/xpra.sock" ] && DISPLAY=` + appDisplay + ` xset q >/dev/null 2>&1; then exit 0; fi
  systemctl --user is-active --quiet ` + appServerUnit + ` || break
  sleep 0.5
done
echo "xpra server did not come up:" >&2
journalctl --user -u ` + appServerUnit + ` -n 30 --no-pager >&2 || true
exit 1
`
}

// webappProfile writes a Firefox profile for a single-site window: no
// tab strip or toolbars, no session restore or first-run pages.
const webappProfile = `mkdir -p "$profile/chrome"
cat > "$profile/user.js" <<'EOF'
user_pref("toolkit.legacyUserProfileCustomizations.stylesheets", true);
user_pref("browser.sessionstore.resume_from_crash", false);
user_pref("browser.startup.homepage_override.mstone", "ignore");
user_pref("browser.aboutwelcome.enabled", false);
user_pref("browser.shell.checkDefaultBrowser", false);
user_pref("datareporting.policy.dataSubmissionEnabled", false);
user_pref("toolkit.telemetry.reportingpolicy.firstRun", false);
user_pref("app.update.auto", false);
user_pref("browser.preonboarding.enabled", false);
user_pref("termsofuse.bypassNotification", true);
user_pref("datareporting.policy.dataSubmissionPolicyBypassNotification", true);
EOF
cat > "$profile/chrome/userChrome.css" <<'EOF'
#TabsToolbar, #nav-bar, #PersonalToolbar, #titlebar { visibility: collapse !important; }
EOF
`

// appLaunchScript starts app as a user unit on the Xpra display and fails
// if it exits within the first seconds.
func appLaunchScript(a *appRecord) string {
	unit := appUnitPrefix + a.ID
	argv := a.Argv
	prep := ""
	if a.Kind == "url" {
		prep = `profile="$HOME/.local/share/pvm/apps/` + a.ID + `/profile"
` + webappProfile
		argv = []string{"firefox", "--new-instance", "--no-remote", "--profile", "$profile", "--class", unit, "--name", unit}
		if a.Kiosk {
			argv = append(argv, "--kiosk")
		}
		argv = append(argv, a.URL)
	}
	words := make([]string, len(argv))
	for i, w := range argv {
		if w == "$profile" {
			words[i] = `"$profile"`
		} else {
			words[i] = shellQuote(w)
		}
	}
	return `set -e
if ! command -v ` + shellQuote(argv[0]) + ` >/dev/null 2>&1; then
  echo "NOT_INSTALLED"
  exit 3
fi
` + prep + `systemctl --user reset-failed ` + unit + ` >/dev/null 2>&1 || true
systemd-run --user --quiet --collect --unit=` + unit + ` -E DISPLAY=` + appDisplay + ` -- ` + strings.Join(words, " ") + `
sleep 2
if ! systemctl --user is-active --quiet ` + unit + `; then
  echo "app exited right away:" >&2
  journalctl --user -u ` + unit + ` -n 30 --no-pager >&2 || true
  exit 1
fi
`
}

// appID derives an id from the command or URL when none is given.
func appID(req *appStartRequest) string {
	base := ""
	if req.URL != "" {
		if u, err := url.Parse(req.URL); err == nil {
			base = strings.TrimPrefix(u.Hostname(), "www.")
		}
	} else if len(req.Argv) > 0 {
		base = path.Base(req.Argv[0])
	}
	var b strings.Builder
	for _, r := range strings.ToLower(base) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteByte('-')
		}
	}
	id := strings.Trim(b.String(), "-")
	if len(id) > 24 {
		id = strings.Trim(id[:24], "-")
	}
	if id == "" {
		id = "app"
	}
	return id
}

func validateAppRequest(req *appStartRequest) error {
	if (req.URL == "") == (len(req.Argv) == 0) {
		return fmt.Errorf("give either a URL or a command")
	}
	if req.URL != "" {
		u, err := url.Parse(req.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("invalid URL %q: want http:// or https://", req.URL)
		}
	}
	for _, w := range req.Argv {
		if strings.ContainsRune(w, 0) {
			return fmt.Errorf("command arguments must not contain NUL")
		}
	}
	if len(req.Argv) > 0 && req.Argv[0] == "" {
		return fmt.Errorf("empty command")
	}
	if req.ID != "" && !appIDRe.MatchString(req.ID) {
		return fmt.Errorf("invalid app name %q: lowercase letters, digits and -, at most 32", req.ID)
	}
	if req.Kiosk && req.URL == "" {
		return fmt.Errorf("--kiosk applies to URLs only")
	}
	return nil
}

func (s *Server) appVM(w http.ResponseWriter, name string) (*vm.Spec, bool) {
	spec, err := s.getSpec(name)
	if err != nil {
		writeErr(w, 404, err)
		return nil, false
	}
	if spec.Role == "gateway" || spec.ClusterID == 0 {
		writeErr(w, 400, fmt.Errorf("vm %s is not a cluster vm", name))
		return nil, false
	}
	if st, err := s.instance(spec).Running(); err != nil {
		writeErr(w, 500, err)
		return nil, false
	} else if st == nil {
		writeErr(w, 409, fmt.Errorf("vm %s is not running; run `pvm start %s`", name, name))
		return nil, false
	}
	return spec, true
}

// ensureAppServer installs and starts the VM's Xpra server.
func (s *Server) ensureAppServer(ctx context.Context, spec *vm.Spec) error {
	if out, err := s.sshRunCtx(ctx, spec, appInstallScript()); err != nil {
		return fmt.Errorf("install xpra in %s: %v%s", spec.Name, err, indentOutput(out))
	}
	_, secret, err := s.appSecret(spec.Name)
	if err != nil {
		return err
	}
	if out, err := s.sshRunInputCtx(ctx, spec, appServerScript(), secret); err != nil {
		return fmt.Errorf("start xpra server in %s: %v%s", spec.Name, err, indentOutput(out))
	}
	return nil
}

// ensureAppForward starts the host listener for the VM's Xpra server.
func (s *Server) ensureAppForward(spec *vm.Spec, port int) (*streamForward, error) {
	s.apps.mu.Lock()
	f := s.apps.fwd[spec.Name]
	s.apps.mu.Unlock()
	if f != nil {
		return f, nil
	}
	target := "xpra:" + strconv.Itoa(spec.GuestUID)
	f, err := s.startStreamForward(spec.Name, target, "127.0.0.1", port)
	if err != nil && port != 0 {
		f, err = s.startStreamForward(spec.Name, target, "127.0.0.1", 0)
	}
	if err != nil {
		return nil, err
	}
	s.apps.mu.Lock()
	if s.apps.fwd == nil {
		s.apps.fwd = make(map[string]*streamForward)
	}
	s.apps.fwd[spec.Name] = f
	s.apps.mu.Unlock()
	return f, nil
}

func (s *Server) stopAppForward(name string) {
	s.apps.mu.Lock()
	f := s.apps.fwd[name]
	delete(s.apps.fwd, name)
	delete(s.apps.pending, name)
	s.apps.mu.Unlock()
	if f != nil {
		f.Close()
	}
}

// recoverAppForwards restores the listeners of VMs with apps on their
// recorded ports, so attached clients reconnect after a daemon restart.
func (s *Server) recoverAppForwards(specs []*vm.Spec) {
	for _, spec := range specs {
		st, err := s.loadAppState(spec.Name)
		if err != nil || st.Port == 0 || len(st.Apps) == 0 {
			continue
		}
		if _, err := s.ensureAppForward(spec, st.Port); err != nil {
			log.Printf("app %s: restore listener on %d: %v", spec.Name, st.Port, err)
		}
	}
}

// attachInfo reports whether a client is attached, and when none is,
// reserves the start of one for the caller. force reports none.
func (s *Server) attachInfo(spec *vm.Spec, st *appState, f *streamForward, force bool) (appAttach, error) {
	pw, _, err := s.appSecret(spec.Name)
	if err != nil {
		return appAttach{}, err
	}
	s.apps.mu.Lock()
	defer s.apps.mu.Unlock()
	connected := !force && (f.active.Load() > 0 || time.Since(s.apps.pending[spec.Name]) < appAttachGrace)
	if !connected {
		if s.apps.pending == nil {
			s.apps.pending = make(map[string]time.Time)
		}
		s.apps.pending[spec.Name] = time.Now()
	}
	return appAttach{Port: f.port(), PasswordFile: pw, Title: "@title@ [" + spec.Name + "]", Clipboard: st.clipboard(), Drop: st.Drop, Connected: connected}, nil
}

func (s *Server) handleAppStart(w http.ResponseWriter, r *http.Request) {
	spec, ok := s.appVM(w, r.PathValue("name"))
	if !ok {
		return
	}
	var req appStartRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, fmt.Errorf("bad request: %v", err))
		return
	}
	if err := validateAppRequest(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	lock := s.appLock(spec.Name)
	lock.Lock()
	defer lock.Unlock()
	st, err := s.loadAppState(spec.Name)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Minute)
	defer cancel()
	running := s.appUnitsActive(ctx, spec)
	st.Apps = slices.DeleteFunc(st.Apps, func(a appRecord) bool { return running != nil && !running[a.ID] })
	id := req.ID
	if id == "" {
		id = appID(&req)
		for n := 2; slices.ContainsFunc(st.Apps, func(a appRecord) bool { return a.ID == id }); n++ {
			id = fmt.Sprintf("%s-%d", appID(&req), n)
		}
	} else if slices.ContainsFunc(st.Apps, func(a appRecord) bool { return a.ID == id }) {
		writeErr(w, 409, fmt.Errorf("app %s already runs in %s; stop it with `pvm app stop %s %s`", id, spec.Name, spec.Name, id))
		return
	}
	kind := "cmd"
	if req.URL != "" {
		kind = "url"
	}
	rec := appRecord{ID: id, Kind: kind, Argv: req.Argv, URL: req.URL, Kiosk: req.Kiosk, Created: time.Now().UTC()}
	if err := s.ensureAppServer(ctx, spec); err != nil {
		writeErr(w, 500, err)
		return
	}
	// record first, so a failure below is rolled back from a known state
	st.Apps = append(st.Apps, rec)
	if err := s.saveAppState(spec.Name, st); err != nil {
		writeErr(w, 500, err)
		return
	}
	rollback := func() {
		s.sshRunCtx(context.Background(), spec, "systemctl --user stop "+appUnitPrefix+id+" >/dev/null 2>&1 || true")
		st.Apps = slices.DeleteFunc(st.Apps, func(a appRecord) bool { return a.ID == id })
		if err := s.saveAppState(spec.Name, st); err != nil {
			log.Printf("app %s/%s: roll back record: %v", spec.Name, id, err)
		}
	}
	if out, err := s.sshRunCtx(ctx, spec, appLaunchScript(&rec)); err != nil {
		rollback()
		if strings.Contains(out, "NOT_INSTALLED") {
			prog := rec.Argv
			if kind == "url" {
				prog = []string{"firefox"}
			}
			writeErr(w, 400, fmt.Errorf("%s is not installed in %s; install it first, e.g. `pvm exec %s sudo apt-get install -y %s`", prog[0], spec.Name, spec.Name, path.Base(prog[0])))
			return
		}
		writeErr(w, 500, fmt.Errorf("start %s: %v%s", id, err, indentOutput(out)))
		return
	}
	f, err := s.ensureAppForward(spec, st.Port)
	if err != nil {
		rollback()
		writeErr(w, 500, err)
		return
	}
	if st.Port != f.port() {
		st.Port = f.port()
		if err := s.saveAppState(spec.Name, st); err != nil {
			writeErr(w, 500, err)
			return
		}
	}
	att, err := s.attachInfo(spec, st, f, false)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 201, appStartResponse{App: appView{VM: spec.Name, appRecord: rec, State: "running"}, Attach: att})
}

// appUnitsActive returns which app units run, or nil when it cannot tell.
func (s *Server) appUnitsActive(ctx context.Context, spec *vm.Spec) map[string]bool {
	out, err := s.sshRunCtx(ctx, spec, "systemctl --user list-units --no-legend --plain --state=active '"+appUnitPrefix+"*' 2>/dev/null || true")
	if err != nil {
		return nil
	}
	active := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		if id, ok := strings.CutPrefix(strings.TrimSuffix(f[0], ".service"), appUnitPrefix); ok {
			active[id] = true
		}
	}
	return active
}

// listApps reports a VM's apps. Exited ones are reported once and then
// dropped from the record.
func (s *Server) listApps(ctx context.Context, spec *vm.Spec) ([]appView, error) {
	lock := s.appLock(spec.Name)
	lock.Lock()
	defer lock.Unlock()
	st, err := s.loadAppState(spec.Name)
	if err != nil || len(st.Apps) == 0 {
		return nil, err
	}
	var active map[string]bool
	if run, err := s.instance(spec).Running(); err == nil && run != nil {
		active = s.appUnitsActive(ctx, spec)
	} else {
		active = map[string]bool{}
	}
	views := make([]appView, 0, len(st.Apps))
	for _, a := range st.Apps {
		state := "unknown"
		if active != nil {
			state = "exited"
			if active[a.ID] {
				state = "running"
			}
		}
		views = append(views, appView{VM: spec.Name, appRecord: a, State: state})
	}
	if active != nil {
		kept := slices.DeleteFunc(slices.Clone(st.Apps), func(a appRecord) bool { return !active[a.ID] })
		if len(kept) != len(st.Apps) {
			st.Apps = kept
			if err := s.saveAppState(spec.Name, st); err != nil {
				return views, err
			}
		}
	}
	return views, nil
}

func (s *Server) handleAppList(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var specs []*vm.Spec
	if name != "" {
		spec, err := s.getSpec(name)
		if err != nil {
			writeErr(w, 404, err)
			return
		}
		specs = []*vm.Spec{spec}
	} else {
		// a corrupt spec elsewhere must not hide the apps of the others
		all, err := vm.ListAll(s.P)
		if err != nil {
			log.Printf("app list: %v", err)
		}
		for _, spec := range all {
			if spec.Role != "gateway" {
				specs = append(specs, spec)
			}
		}
		slices.SortFunc(specs, func(a, b *vm.Spec) int { return strings.Compare(a.Name, b.Name) })
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	views := []appView{}
	for _, spec := range specs {
		v, err := s.listApps(ctx, spec)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		views = append(views, v...)
	}
	writeJSON(w, 200, views)
}

func (s *Server) handleAppStop(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	spec, err := s.getSpec(name)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	id := r.PathValue("id")
	lock := s.appLock(name)
	lock.Lock()
	defer lock.Unlock()
	st, err := s.loadAppState(name)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	var stop []string
	for _, a := range st.Apps {
		if id == "" || a.ID == id {
			stop = append(stop, a.ID)
		}
	}
	if id != "" && len(stop) == 0 {
		writeErr(w, 404, fmt.Errorf("no app %s in %s", id, name))
		return
	}
	run, err := s.instance(spec).Running()
	vmUp := err == nil && run != nil
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	if vmUp && len(stop) > 0 {
		units := make([]string, len(stop))
		for i, a := range stop {
			units[i] = appUnitPrefix + a
		}
		if out, err := s.sshRunCtx(ctx, spec, "systemctl --user stop "+strings.Join(units, " ")+" 2>&1 || true"); err != nil {
			writeErr(w, 500, fmt.Errorf("stop apps in %s: %v%s", name, err, indentOutput(out)))
			return
		}
	}
	st.Apps = slices.DeleteFunc(st.Apps, func(a appRecord) bool { return slices.Contains(stop, a.ID) })
	if err := s.saveAppState(name, st); err != nil {
		writeErr(w, 500, err)
		return
	}
	if len(st.Apps) == 0 {
		// the last app is gone: the server and its client go too
		if vmUp {
			if out, err := s.sshRunCtx(ctx, spec, "systemctl --user stop "+appServerUnit+" 2>&1 || true"); err != nil {
				writeErr(w, 500, fmt.Errorf("stop xpra server in %s: %v%s", name, err, indentOutput(out)))
				return
			}
		}
		s.stopAppForward(name)
	}
	writeJSON(w, 200, map[string]any{"stopped": stop})
}

func (s *Server) handleAppAttach(w http.ResponseWriter, r *http.Request) {
	spec, ok := s.appVM(w, r.PathValue("name"))
	if !ok {
		return
	}
	lock := s.appLock(spec.Name)
	lock.Lock()
	defer lock.Unlock()
	st, err := s.loadAppState(spec.Name)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if len(st.Apps) == 0 {
		writeErr(w, 404, fmt.Errorf("no apps run in %s; start one with `pvm app %s -- <command>`", spec.Name, spec.Name))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if err := s.ensureAppServer(ctx, spec); err != nil {
		writeErr(w, 500, err)
		return
	}
	f, err := s.ensureAppForward(spec, st.Port)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if st.Port != f.port() {
		st.Port = f.port()
		if err := s.saveAppState(spec.Name, st); err != nil {
			writeErr(w, 500, err)
			return
		}
	}
	att, err := s.attachInfo(spec, st, f, r.URL.Query().Get("force") == "1")
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, att)
}

// handleAppConfig sets a VM's app policy. A running client keeps the old
// one until it is attached again.
func (s *Server) handleAppConfig(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := s.getSpec(name); err != nil {
		writeErr(w, 404, err)
		return
	}
	var req struct {
		Clipboard string `json:"clipboard"`
		Drop      *bool  `json:"drop"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, fmt.Errorf("bad request: %v", err))
		return
	}
	lock := s.appLock(name)
	lock.Lock()
	defer lock.Unlock()
	st, err := s.loadAppState(name)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if req.Clipboard != "" {
		if !slices.Contains(clipboardModes, req.Clipboard) {
			writeErr(w, 400, fmt.Errorf("--clipboard must be one of %s", strings.Join(clipboardModes, ", ")))
			return
		}
		st.Clipboard = req.Clipboard
		if st.Clipboard == "off" {
			st.Clipboard = ""
		}
	}
	if req.Drop != nil {
		st.Drop = *req.Drop
	}
	if err := s.saveAppState(name, st); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"clipboard": st.clipboard(), "drop": st.Drop})
}

// clipTarget turns the API's clip:<kind> into the guest's
// clip:<uid>:<kind>, refused unless the VM lets the host clipboard in.
func (s *Server) clipTarget(spec *vm.Spec, target string) (string, int, error) {
	kind, ok := strings.CutPrefix(target, "clip:")
	if !ok {
		return target, 0, nil
	}
	if kind != "text" && kind != "png" {
		return "", 400, fmt.Errorf("unknown clipboard kind %q", kind)
	}
	st, err := s.loadAppState(spec.Name)
	if err != nil {
		return "", 500, err
	}
	if !st.clipToGuest() {
		return "", 403, fmt.Errorf("the host clipboard may not enter %s; allow it with `pvm app config %s --clipboard to-guest`", spec.Name, spec.Name)
	}
	return "clip:" + strconv.Itoa(spec.GuestUID) + ":" + kind, 0, nil
}
