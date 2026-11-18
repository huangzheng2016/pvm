package daemon

import (
	"context"
	_ "embed"
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
	"unicode"

	"github.com/huangzheng2016/pvm/internal/fsutil"
	"github.com/huangzheng2016/pvm/internal/image"
	"github.com/huangzheng2016/pvm/internal/vm"
	"github.com/huangzheng2016/pvm/internal/workspace"
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
	// All web apps of a VM are windows of one Firefox, so they share
	// logins, cookies and cache; a profile allows only one process.
	webappUnit  = "pvm-webapps"
	webappClass = "pvm-webapp"
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
	// Window is the X window of a web app in the shared Firefox.
	Window string `json:"window,omitempty"`
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
	// Audio is out (the default: VM sound plays on the host), both (the
	// host microphone also reaches the VM) or off.
	Audio string `json:"audio,omitempty"`
	// Installed are the catalog apps `pvm app install` put into the VM.
	Installed []string `json:"installed,omitempty"`
}

var clipboardModes = []string{"off", "to-guest", "to-host", "both"}

var audioModes = []string{"off", "out", "both"}

func (st *appState) audio() string {
	if st.Audio == "" {
		return "out"
	}
	return st.Audio
}

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

// clipToHost reports whether the VM's clipboard content may reach the host.
func (st *appState) clipToHost() bool {
	return st.Clipboard == "to-host" || st.Clipboard == "both"
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
	Audio        string `json:"audio"`
	// Connected is true while a client is attached or one was started in
	// the last appAttachGrace; the CLI then starts no second client.
	Connected bool `json:"connected"`
}

type appStartRequest struct {
	ID    string   `json:"id"`
	Argv  []string `json:"argv"`
	URL   string   `json:"url"`
	Kiosk bool     `json:"kiosk"`
	// Cwd is the guest directory a command starts in, inside the
	// workspace CwdRoot. Both are empty outside a workspace.
	Cwd     string `json:"cwd"`
	CwdRoot string `json:"cwd_root"`
}

type appStartResponse struct {
	App    appView   `json:"app"`
	Attach appAttach `json:"attach"`
	Note   string    `json:"note,omitempty"`
}

const appAttachGrace = 20 * time.Second

type appHost struct {
	mu      sync.Mutex
	fwd     map[string]*streamForward
	pending map[string]time.Time // last client start per VM
	locks   sync.Map             // vm name -> *sync.Mutex
	// vm name -> *sync.Mutex; serialises catalog installs without
	// keeping the app lock, and so the app list, for a script's length
	installs sync.Map
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

// appLatePackages came after the first Xpra install; guests that have the
// rest only add these, and go without one the release lacks or that fails
// to install. The fonts are in golden images too, but VMs baked before
// showed boxes for most scripts and emoji.
var appLatePackages = []struct{ pkg, without string }{
	{"gtk3-nocsd", "GTK apps keep their own title bar"},
	{"xpra-audio-server", "apps have no sound"},
	{"fonts-noto-core", "apps show boxes for scripts other than Latin and CJK"},
	{"fonts-noto-color-emoji", "apps show boxes for emoji"},
}

// appInstallScript installs the Xpra server unless it is there, and with
// late the packages of appLatePackages, which can take minutes. It runs as
// the VM user, who has passwordless sudo.
func appInstallScript(late bool) string {
	const apt = "sudo -n env DEBIAN_FRONTEND=noninteractive apt-get -q -o DPkg::Lock::Timeout=300"
	script := "{ { command -v xpra && command -v xclip && command -v wmctrl && command -v xdotool; } >/dev/null 2>&1 || { sudo -n bash -euo pipefail -c " + shellQuote(image.XpraRepoScript) +
		" && " + apt + " update && " + apt + " install -y --no-install-recommends " + strings.Join(image.XpraPackages, " ") + "; }; }"
	if late {
		for _, l := range appLatePackages {
			script += " && { dpkg -s " + l.pkg + " >/dev/null 2>&1 || " + apt + " install -y --no-install-recommends " + l.pkg + " || { " + apt + " update && " + apt + " install -y --no-install-recommends " + l.pkg + "; } || echo '" + l.pkg + " is unavailable; " + l.without + "' >&2; }"
		}
	}
	// the mask also runs where xpra came preinstalled
	return script + " && { [ \"$(systemctl is-enabled xpra-server.socket 2>/dev/null)\" = masked ] || ! systemctl cat xpra-server.socket >/dev/null 2>&1 || sudo -n sh -c " + shellQuote(image.XpraMaskScript) + "; }"
}

// xpraStartPy starts the Xpra server and types text a host input method
// commits; see the file.
//
//go:embed xpra_start.py
var xpraStartPy string

// keyringUnlockPy unlocks the Secret Service keyring for the apps; see
// the file.
//
//go:embed keyring_unlock.py
var keyringUnlockPy string

// appServerScript starts the Xpra server unit unless it runs, and waits
// for its display. The password arrives on stdin. Linger keeps the user
// manager, and with it the server, alive after the ssh session ends.
func appServerScript() string {
	opts := []string{
		// the server runs its own PulseAudio; the client decides whether
		// sound and the microphone flow
		"--daemon=no", "--mdns=no", "--pulseaudio=yes", "--speaker=on", "--microphone=on",
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
cat > "$d/xpra-start.py" <<'PVMEOF'
` + xpraStartPy + `PVMEOF
# services D-Bus starts for apps, such as the desktop portal's GTK
# backend, need the display; without it the portal hung until a timeout
# and GTK apps waited a minute for it before showing a window. Apps play
# sound into the PulseAudio of the Xpra server, not the session's.
env="DISPLAY=` + appDisplay + `"
command -v pulseaudio >/dev/null && env="$env PULSE_SERVER=unix:${XDG_RUNTIME_DIR:-/run/user/$(id -u)}/xpra/` + strings.TrimPrefix(appDisplay, ":") + `/pulse/native"
dbus-update-activation-environment --systemd $env 2>/dev/null || systemctl --user set-environment $env
systemctl --user reset-failed xdg-desktop-portal.service xdg-desktop-portal-gtk.service >/dev/null 2>&1 || true
if command -v gnome-keyring-daemon >/dev/null; then
  python3 - <<'PVMEOF' || echo "pvm: could not unlock the keyring; apps may ask for its password" >&2
` + keyringUnlockPy + `PVMEOF
fi
if ! systemctl --user is-active --quiet ` + appServerUnit + `; then
  systemctl --user reset-failed ` + appServerUnit + ` >/dev/null 2>&1 || true
  rm -f "$d/xpra.sock"
  # the server starts that PulseAudio, which refuses to while PULSE_SERVER
  # names a server
  systemd-run --user --quiet --collect --unit=` + appServerUnit + ` -E DISPLAY=` + appDisplay + ` -- env -u PULSE_SERVER python3 "$d/xpra-start.py" start ` + appDisplay + ` --bind="$d/xpra.sock,auth=file:filename=$d/xpra.secret" ` + strings.Join(opts, " ") + `
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

// webappProfile writes the shared web app profile: no tab strip, toolbar
// or first-run pages, and links that open a new tab open a new window,
// because tabs are hidden. Popups, marked popup-window by Firefox, keep
// their read-only address bar, so a sign-in window shows where it is.
const webappProfile = `profile="$HOME/.local/share/pvm/webapps/profile"
mkdir -p "$profile/chrome"
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
user_pref("browser.link.open_newwindow", 2);
EOF
cat > "$profile/chrome/userChrome.css" <<'EOF'
#TabsToolbar, #PersonalToolbar { visibility: collapse !important; }
#star-button-box { display: none !important; }
:root:not([popup-window]) #nav-bar { visibility: collapse !important; }
EOF
`

// webappWindows lists the shared Firefox's browser windows.
const webappWindows = `webapp_windows() { wmctrl -lx 2>/dev/null | awk '$3 == "Navigator.` + webappClass + `" {print $1}' | sort; }
`

// webappLaunchScript opens a URL as a new window of the VM's shared
// Firefox, starting it if needed, and prints the window as WINDOW=<id>.
func webappLaunchScript(a *appRecord) string {
	url := shellQuote(a.URL)
	kiosk := ""
	if a.Kiosk {
		kiosk = `wmctrl -i -r "$win" -b add,fullscreen
`
	}
	return `set -e
if ! command -v firefox >/dev/null 2>&1; then
  echo "NOT_INSTALLED"
  exit 3
fi
export DISPLAY=` + appDisplay + `
` + webappProfile + webappWindows + `before=$(mktemp)
webapp_windows > "$before"
if systemctl --user is-active --quiet ` + webappUnit + `; then
  firefox --profile "$profile" --new-window ` + url + ` >/dev/null 2>&1
else
  systemctl --user reset-failed ` + webappUnit + ` >/dev/null 2>&1 || true
  systemd-run --user --quiet --collect --unit=` + webappUnit + ` -E DISPLAY=` + appDisplay + ` -- firefox --profile "$profile" --class ` + webappClass + ` --name ` + webappClass + ` ` + url + `
fi
win=
for i in $(seq 1 60); do
  win=$(webapp_windows | grep -vxF -f "$before" | head -1 || true)
  [ -n "$win" ] && break
  sleep 0.5
done
rm -f "$before"
if [ -z "$win" ]; then
  echo "no firefox window appeared:" >&2
  journalctl --user -u ` + webappUnit + ` -n 30 --no-pager >&2 || true
  exit 1
fi
` + kiosk + `echo "WINDOW=$win"
`
}

// appStopScript stops apps: their units, and the windows of web apps.
// The shared Firefox stops once no web app is left.
func appStopScript(stop []appRecord, webappsLeft bool) string {
	var units, wins []string
	for _, a := range stop {
		units = append(units, appUnitPrefix+a.ID)
		if a.Window != "" {
			wins = append(wins, shellQuote(a.Window))
		}
	}
	script := "systemctl --user stop " + strings.Join(units, " ") + " 2>/dev/null || true\n"
	if len(wins) > 0 {
		script += "for w in " + strings.Join(wins, " ") + "; do DISPLAY=" + appDisplay + ` wmctrl -i -c "$w" 2>/dev/null || true; done` + "\n"
	}
	if !webappsLeft {
		script += "systemctl --user stop " + webappUnit + " 2>/dev/null || true\n"
	}
	return script
}

// appNoCSD sets nocsd to the systemd-run option that preloads
// gtk3-nocsd, so GTK apps leave their title bar and minimize, maximize
// and close buttons to the host window manager, like Firefox does. The
// library is libgtk3-nocsd.so.0 up to version 3 and libgtk-nocsd.so.0
// after; without either, apps keep drawing their own.
const appNoCSD = `nocsd=
for lib in libgtk-nocsd.so.0 libgtk3-nocsd.so.0; do
  if /sbin/ldconfig -p | grep -q "^[[:space:]]*$lib "; then nocsd="-E LD_PRELOAD=$lib"; break; fi
done
`

// appChromium sets nogpu to --disable-gpu when the program is a
// Chromium build, such as Electron apps and Chrome, which ship V8's
// startup snapshot next to the binary. The VM has no usable GPU, so
// Chromium would emulate OpenGL in software and then composite, which is
// slower than its plain software drawing: resizing, scrolling and hover
// lagged. A launcher script that execs Chromium is not recognized.
const appChromium = `nogpu=
bin=$(readlink -f "$(command -v "$1")")
[ -f "${bin%/*}/v8_context_snapshot.bin" ] && nogpu=--disable-gpu
`

// Lines the launch script prints about the working directory.
const (
	appCwdFallback = "CWD_FALLBACK"
	appCwdMissing  = "NO_WORKSPACE"
)

// appWorkDir sets dir to cwd. A sync workspace may not have every host
// folder yet, so a missing cwd falls back to root and says so on stdout;
// without root the app does not start.
func appWorkDir(cwd, root string) string {
	return `dir=` + shellQuote(cwd) + `
if [ ! -d "$dir" ]; then
  dir=` + shellQuote(root) + `
  if [ ! -d "$dir" ]; then
    echo "` + appCwdMissing + `"
    exit 4
  fi
  echo "` + appCwdFallback + `"
fi
`
}

// appLaunchScript starts app as a user unit on the Xpra display and fails
// if it exits within the first seconds; web apps go to webappLaunchScript.
// A command starts in cwd when one is given.
func appLaunchScript(a *appRecord, cwd, root string) string {
	if a.Kind == "url" {
		return webappLaunchScript(a)
	}
	unit := appUnitPrefix + a.ID
	workDir, workDirOpt := "", ""
	if cwd != "" {
		workDir, workDirOpt = appWorkDir(cwd, root), ` --working-directory="$dir"`
	}
	words := make([]string, len(a.Argv))
	for i, w := range a.Argv {
		words[i] = shellQuote(w)
	}
	return `set -e
set -- ` + strings.Join(words, " ") + `
if ! command -v "$1" >/dev/null 2>&1; then
  echo "NOT_INSTALLED"
  exit 3
fi
` + workDir + appNoCSD + appChromium + `prog=$1
shift
systemctl --user reset-failed ` + unit + ` >/dev/null 2>&1 || true
systemd-run --user --quiet --collect --unit=` + unit + workDirOpt + ` -E DISPLAY=` + appDisplay + ` $nocsd -- "$prog" $nogpu "$@"
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
	return validateAppCwd(req)
}

// validateAppCwd accepts a workspace's guest root and a directory in it.
// Host folder names end up in cwd, so spaces and non-ASCII are allowed.
func validateAppCwd(req *appStartRequest) error {
	if req.Cwd == "" && req.CwdRoot == "" {
		return nil
	}
	if req.URL != "" {
		return fmt.Errorf("cwd applies to commands only")
	}
	id, ok := strings.CutPrefix(req.CwdRoot, workspace.GuestRoot+"/")
	if !ok || workspace.ValidID(id) != nil {
		return fmt.Errorf("invalid cwd_root %q: want %s/<workspace>", req.CwdRoot, workspace.GuestRoot)
	}
	if req.Cwd != req.CwdRoot && !strings.HasPrefix(req.Cwd, req.CwdRoot+"/") {
		return fmt.Errorf("invalid cwd %q: not inside %s", req.Cwd, req.CwdRoot)
	}
	if req.Cwd != path.Clean(req.Cwd) || strings.ContainsFunc(req.Cwd, unicode.IsControl) {
		return fmt.Errorf("invalid cwd %q: want a clean path without control characters", req.Cwd)
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
	if out, err := s.sshRunCtx(ctx, spec, appInstallScript(false)); err != nil {
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

// closeAppClients stops the VM's Xpra server before the VM goes down. The
// server tells attached clients that it shuts down, and they exit; they
// would otherwise keep reconnecting to a VM that is gone, with a window
// left on the host.
func (s *Server) closeAppClients(spec *vm.Spec) {
	s.apps.mu.Lock()
	f := s.apps.fwd[spec.Name]
	s.apps.mu.Unlock()
	if f == nil {
		return
	}
	if run, err := s.instance(spec).Running(); err == nil && run != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if out, err := s.sshRunCtx(ctx, spec, "systemctl --user stop "+appServerUnit+" 2>&1 || true"); err != nil {
			log.Printf("stop xpra server in %s: %v%s", spec.Name, err, indentOutput(out))
		}
	}
	s.stopAppForward(spec.Name)
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
	return appAttach{Port: f.port(), PasswordFile: pw, Title: "@title@ [" + spec.Name + "]", Clipboard: st.clipboard(), Drop: st.Drop, Audio: st.audio(), Connected: connected}, nil
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
	running := s.appUnitsActive(ctx, spec, st.Apps)
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
		st.Apps = slices.DeleteFunc(st.Apps, func(a appRecord) bool { return a.ID == id })
		s.sshRunCtx(context.Background(), spec, appStopScript([]appRecord{rec}, st.hasWebapps()))
		if err := s.saveAppState(spec.Name, st); err != nil {
			log.Printf("app %s/%s: roll back record: %v", spec.Name, id, err)
		}
	}
	out, err := s.sshRunCtx(ctx, spec, appLaunchScript(&rec, req.Cwd, req.CwdRoot))
	if err != nil {
		rollback()
		if hasLine(out, appCwdMissing) {
			writeErr(w, 400, fmt.Errorf("%s is not in %s", req.CwdRoot, spec.Name))
			return
		}
		if strings.Contains(out, "NOT_INSTALLED") {
			prog := rec.Argv
			if kind == "url" {
				prog = []string{"firefox"}
			}
			if id, ok := catalogAppFor(prog[0]); ok {
				writeErr(w, 400, fmt.Errorf("%s is not installed in %s; install it with `pvm app install %s %s`", prog[0], spec.Name, spec.Name, id))
				return
			}
			writeErr(w, 400, fmt.Errorf("%s is not installed in %s; install it first, e.g. `pvm exec %s sudo apt-get install -y %s`", prog[0], spec.Name, spec.Name, path.Base(prog[0])))
			return
		}
		writeErr(w, 500, fmt.Errorf("start %s: %v%s", id, err, indentOutput(out)))
		return
	}
	if kind == "url" {
		win, ok := webappWindow(out)
		if !ok {
			rollback()
			writeErr(w, 500, fmt.Errorf("start %s: no window id in %q", id, strings.TrimSpace(out)))
			return
		}
		rec.Window = win
		st.Apps[len(st.Apps)-1].Window = win
		if err := s.saveAppState(spec.Name, st); err != nil {
			rollback()
			writeErr(w, 500, err)
			return
		}
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
	note := ""
	if hasLine(out, appCwdFallback) {
		note = fmt.Sprintf("%s is not in the vm; starting in %s", req.Cwd, req.CwdRoot)
	}
	writeJSON(w, 201, appStartResponse{App: appView{VM: spec.Name, appRecord: rec, State: "running"}, Attach: att, Note: note})
}

func hasLine(out, line string) bool {
	return slices.Contains(strings.Split(strings.ReplaceAll(out, "\r", ""), "\n"), line)
}

// appUnitsActive returns which apps run, or nil when it cannot tell: an
// app with its own unit runs while the unit is active, a web app while
// its window exists in the shared Firefox.
func (s *Server) appUnitsActive(ctx context.Context, spec *vm.Spec, apps []appRecord) map[string]bool {
	out, err := s.sshRunCtx(ctx, spec, "systemctl --user list-units --no-legend --plain --state=active '"+appUnitPrefix+"*' 2>/dev/null || true; echo ---; "+webappWindows+"DISPLAY="+appDisplay+" webapp_windows")
	if err != nil {
		return nil
	}
	units, wins, _ := strings.Cut(out, "---")
	active := map[string]bool{}
	for _, l := range strings.Split(units, "\n") {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		if id, ok := strings.CutPrefix(strings.TrimSuffix(f[0], ".service"), appUnitPrefix); ok {
			active[id] = true
		}
	}
	open := strings.Fields(wins)
	for _, a := range apps {
		if a.Window != "" && slices.Contains(open, a.Window) {
			active[a.ID] = true
		}
	}
	return active
}

// webappWindow reads the WINDOW= line of webappLaunchScript.
func webappWindow(out string) (string, bool) {
	for _, l := range strings.Split(out, "\n") {
		if w, ok := strings.CutPrefix(strings.TrimSpace(l), "WINDOW="); ok && strings.HasPrefix(w, "0x") {
			return w, true
		}
	}
	return "", false
}

func (st *appState) hasWebapps() bool {
	return slices.ContainsFunc(st.Apps, func(a appRecord) bool { return a.Kind == "url" })
}

// listApps reports a VM's apps and its installed catalog apps. Exited
// apps are reported once and then dropped from the record.
func (s *Server) listApps(ctx context.Context, spec *vm.Spec) ([]appView, []string, error) {
	lock := s.appLock(spec.Name)
	lock.Lock()
	defer lock.Unlock()
	st, err := s.loadAppState(spec.Name)
	if err != nil {
		return nil, nil, err
	}
	if len(st.Apps) == 0 {
		return nil, st.Installed, nil
	}
	var active map[string]bool
	if run, err := s.instance(spec).Running(); err == nil && run != nil {
		active = s.appUnitsActive(ctx, spec, st.Apps)
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
				return views, st.Installed, err
			}
		}
	}
	return views, st.Installed, nil
}

// appListResponse is the answer of the app list routes. Installed maps a
// VM to its installed catalog apps; a VM without any is absent.
type appListResponse struct {
	Apps      []appView           `json:"apps"`
	Installed map[string][]string `json:"installed"`
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
	out := appListResponse{Apps: []appView{}, Installed: map[string][]string{}}
	for _, spec := range specs {
		v, installed, err := s.listApps(ctx, spec)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		out.Apps = append(out.Apps, v...)
		if len(installed) > 0 {
			out.Installed[spec.Name] = installed
		}
	}
	writeJSON(w, 200, out)
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
	var stopRecs []appRecord
	for _, a := range st.Apps {
		if id == "" || a.ID == id {
			stop = append(stop, a.ID)
			stopRecs = append(stopRecs, a)
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
	st.Apps = slices.DeleteFunc(st.Apps, func(a appRecord) bool { return slices.Contains(stop, a.ID) })
	if vmUp && len(stop) > 0 {
		if out, err := s.sshRunCtx(ctx, spec, appStopScript(stopRecs, st.hasWebapps())); err != nil {
			writeErr(w, 500, fmt.Errorf("stop apps in %s: %v%s", name, err, indentOutput(out)))
			return
		}
	}
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
		Audio     string `json:"audio"`
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
	if req.Audio != "" {
		if !slices.Contains(audioModes, req.Audio) {
			writeErr(w, 400, fmt.Errorf("--audio must be one of %s", strings.Join(audioModes, ", ")))
			return
		}
		st.Audio = req.Audio
		if st.Audio == "out" {
			st.Audio = ""
		}
	}
	if err := s.saveAppState(name, st); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{"clipboard": st.clipboard(), "drop": st.Drop, "audio": st.audio()})
}

// clipTarget turns the API's clip:<kind> into the guest's
// clip:<uid>:<kind>, refused unless the VM lets the host clipboard in, and
// clipout:files, which copies files from the guest clipboard, into
// clipout:<uid>:files, refused unless the VM's clipboard may reach the
// host.
func (s *Server) clipTarget(spec *vm.Spec, target string) (string, int, error) {
	if target == "clipout:files" {
		st, err := s.loadAppState(spec.Name)
		if err != nil {
			return "", 500, err
		}
		if !st.clipToHost() {
			return "", 403, fmt.Errorf("the clipboard of %s may not reach the host; allow it with `pvm app config %s --clipboard to-host`", spec.Name, spec.Name)
		}
		return "clipout:" + strconv.Itoa(spec.GuestUID) + ":files", 0, nil
	}
	kind, ok := strings.CutPrefix(target, "clip:")
	if !ok {
		return target, 0, nil
	}
	if kind != "text" && kind != "png" && kind != "files" {
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

// appPrepareFrame is one line of the prepare stream: output, or the end.
type appPrepareFrame struct {
	Out  string `json:"out,omitempty"`
	Done bool   `json:"done,omitempty"`
	// Installed is set on the last frame when this call installed Xpra.
	Installed bool   `json:"installed,omitempty"`
	Error     string `json:"error,omitempty"`
}

// handleAppPrepare installs Xpra in the VM unless it is there, streaming
// the installer's output as it runs; it takes minutes on first use.
func (s *Server) handleAppPrepare(w http.ResponseWriter, r *http.Request) {
	spec, ok := s.appVM(w, r.PathValue("name"))
	if !ok {
		return
	}
	lock := s.appLock(spec.Name)
	lock.Lock()
	defer lock.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Minute)
	defer cancel()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)
	var mu sync.Mutex
	send := func(f appPrepareFrame) {
		mu.Lock()
		defer mu.Unlock()
		if enc.Encode(f) == nil {
			rc.Flush()
		}
	}
	installed, err := s.prepareAppSupport(ctx, spec, func(l string) { send(appPrepareFrame{Out: l}) })
	if err != nil {
		send(appPrepareFrame{Done: true, Error: err.Error()})
		return
	}
	send(appPrepareFrame{Done: true, Installed: installed})
}

// prepareAppSupport installs Xpra and the late packages unless they are
// there, passing the installer's output to out line by line. It reports
// whether it installed anything.
func (s *Server) prepareAppSupport(ctx context.Context, spec *vm.Spec, out func(string)) (bool, error) {
	if _, err := s.sshRunCtx(ctx, spec, appInstalledCheck()); err == nil {
		return false, nil
	}
	if err := s.sshStreamCtx(ctx, spec, appInstallScript(true), lineWriter(out)); err != nil {
		return false, fmt.Errorf("install xpra in %s: %v", spec.Name, err)
	}
	return true, nil
}

// appInstalledCheck exits 0 when the VM needs no install step. A late
// package the release does not carry does not count.
func appInstalledCheck() string {
	check := `command -v xpra >/dev/null && command -v xclip >/dev/null && command -v wmctrl >/dev/null && command -v xdotool >/dev/null && { [ "$(systemctl is-enabled xpra-server.socket 2>/dev/null)" = masked ] || ! systemctl cat xpra-server.socket >/dev/null 2>&1; }`
	for _, l := range appLatePackages {
		check += " && { dpkg -s " + l.pkg + " >/dev/null 2>&1 || ! apt-cache show " + l.pkg + " >/dev/null 2>&1; }"
	}
	return check
}

// lineWriter calls fn for every complete line written, and for the rest
// on Close. Carriage returns from progress bars end a line too.
type lineWriterT struct {
	mu  sync.Mutex
	buf []byte
	fn  func(string)
}

func lineWriter(fn func(string)) *lineWriterT { return &lineWriterT{fn: fn} }

func (lw *lineWriterT) Write(p []byte) (int, error) {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	lw.buf = append(lw.buf, p...)
	for {
		i := strings.IndexAny(string(lw.buf), "\r\n")
		if i < 0 {
			break
		}
		if l := strings.TrimSpace(string(lw.buf[:i])); l != "" {
			lw.fn(l)
		}
		lw.buf = lw.buf[i+1:]
	}
	return len(p), nil
}

func (lw *lineWriterT) Close() error {
	lw.mu.Lock()
	defer lw.mu.Unlock()
	if l := strings.TrimSpace(string(lw.buf)); l != "" {
		lw.fn(l)
	}
	lw.buf = nil
	return nil
}
