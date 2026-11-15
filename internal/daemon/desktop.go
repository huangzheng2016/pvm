package daemon

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"log"

	"github.com/huangzheng2016/pvm/internal/fsutil"
	"github.com/huangzheng2016/pvm/internal/vm"
)

const (
	desktopVNCDisplay = ":0"
	desktopVNCPort    = 5900 // 5900 + display number
	desktopWebPort    = 6080 // websockify/noVNC listen port inside the guest

	desktopDisableLockScript = `set -e
mkdir -p "$HOME/.config/autostart" "$HOME/.config/xfce4/xfconf/xfce-perchannel-xml"
cat > "$HOME/.config/autostart/xfce4-screensaver.desktop" <<'EOF'
[Desktop Entry]
Type=Application
Name=Screensaver
Exec=xfce4-screensaver
Hidden=true
EOF
cat > "$HOME/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-screensaver.xml" <<'EOF'
<?xml version="1.1" encoding="UTF-8"?>
<channel name="xfce4-screensaver" version="1.0">
  <property name="general" type="empty">
    <property name="enabled" type="bool" value="false"/>
  </property>
</channel>
EOF
pkill -x xfce4-screensaver >/dev/null 2>&1 || true
pkill -x light-locker >/dev/null 2>&1 || true
`
)

var desktopSizeRe = regexp.MustCompile(`^[1-9]\d{2,3}x[1-9]\d{2,3}$`)

// desktopState is the per-VM desktop record. It is persisted under the VM
// dir (0600) so a daemon restart can keep issuing URLs for a live VNC
// session instead of killing it just because the password was forgotten.
type desktopState struct {
	Password string `json:"password"`
	HostPort int    `json:"host_port"`
	Size     string `json:"size"`
	Public   bool   `json:"public"`
}

type desktopResponse struct {
	URL      string `json:"url"`
	HostPort int    `json:"host_port"`
	Public   bool   `json:"public"`
}

func (s *Server) desktopPath(name string) string {
	return s.P.VMDir(name) + "/desktop.json"
}

func (s *Server) loadDesktopState(name string) *desktopState {
	raw, err := os.ReadFile(s.desktopPath(name))
	if err != nil {
		return nil
	}
	var st desktopState
	if err := json.Unmarshal(raw, &st); err == nil && st.Password != "" && st.HostPort > 0 {
		return &st
	}
	return nil
}

func (s *Server) saveDesktopState(name string, st *desktopState) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.AtomicWrite(s.desktopPath(name), raw, 0o600)
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// The noVNC URL carries the VNC password without a daemon-side credential
// proxy. Loopback remains the default; public binding is an explicit opt-in.
func (s *Server) handleDesktop(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	spec, err := s.getSpec(name)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	if spec.Role == "gateway" || spec.ClusterID == 0 {
		writeErr(w, 400, fmt.Errorf("vm %s is not a cluster vm", name))
		return
	}
	size := r.URL.Query().Get("size")
	if size == "" {
		size = "1920x1080"
	}
	if !desktopSizeRe.MatchString(size) {
		writeErr(w, 400, fmt.Errorf("invalid --size %q, want e.g. 1920x1080", size))
		return
	}
	inst := s.instance(spec)
	if st, err := inst.Running(); err != nil {
		writeErr(w, 500, err)
		return
	} else if st == nil {
		writeErr(w, 409, fmt.Errorf("vm %s is not running", name))
		return
	}

	s.deskMu.Lock()
	defer s.deskMu.Unlock()

	st := s.loadDesktopState(name)
	running := s.desktopSessionRunning(spec)

	if out, err := s.sshRun(spec, "command -v vncserver >/dev/null && command -v websockify >/dev/null && echo HAVE_DESKTOP"); err != nil {
		writeErr(w, 503, fmt.Errorf("ssh to %s is not ready yet (%v); the vm may still be booting, retry shortly", name, err))
		return
	} else if !strings.Contains(out, "HAVE_DESKTOP") {
		writeErr(w, 400, fmt.Errorf("desktop packages missing in %s; create the vm from a golden image built with desktop support (pvm image build)", name))
		return
	}
	if out, err := s.sshRun(spec, desktopDisableLockScript); err != nil {
		writeErr(w, 500, fmt.Errorf("disable screen lock: %v\n%s", err, out))
		return
	}

	public := r.URL.Query().Get("public") == "1"
	// maps from before pvm.stream carried the desktop through the gateway
	if err := s.removeDesktopMaps(r.Context(), spec, s.pushGatewayConfigCtx); err != nil {
		writeErr(w, 500, err)
		return
	}
	if !(running && st != nil) {
		st = &desktopState{Password: randHex(8), Size: size}
		if err := s.startDesktopSession(spec, st); err != nil {
			writeErr(w, 500, err)
			return
		}
	}
	port, err := s.ensureDesktopForward(name, st.HostPort, public)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	st.HostPort, st.Public = port, public
	if err := s.saveDesktopState(name, st); err != nil {
		writeErr(w, 500, fmt.Errorf("save desktop state: %v", err))
		return
	}
	writeJSON(w, 200, desktopResponse{URL: desktopURL(st, name), HostPort: st.HostPort, Public: st.Public})
}

// ensureDesktopForward returns the port of the VM's desktop listener,
// starting one on port (0 picks one) when there is none or its binding
// differs. Connections go over pvm.stream, not the gateway.
func (s *Server) ensureDesktopForward(name string, port int, public bool) (int, error) {
	bind := "127.0.0.1"
	if public {
		bind = "0.0.0.0"
	}
	s.mu.Lock()
	cur := s.desktopForwards[name]
	s.mu.Unlock()
	if cur != nil && cur.bind == bind {
		return cur.port(), nil
	}
	if cur != nil {
		cur.Close()
		port = cur.port()
	}
	f, err := s.startStreamForward(name, "novnc", bind, port)
	if err != nil && port != 0 {
		// the old port was taken meanwhile
		f, err = s.startStreamForward(name, "novnc", bind, 0)
	}
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	if s.desktopForwards == nil {
		s.desktopForwards = make(map[string]*streamForward)
	}
	s.desktopForwards[name] = f
	s.mu.Unlock()
	return f.port(), nil
}

func (s *Server) stopDesktopForward(name string) {
	s.mu.Lock()
	f := s.desktopForwards[name]
	delete(s.desktopForwards, name)
	s.mu.Unlock()
	if f != nil {
		f.Close()
	}
}

// recoverDesktopForwards restores the listeners of desktop sessions that
// outlived a daemon restart, on their recorded ports, so issued URLs keep
// working.
func (s *Server) recoverDesktopForwards(specs []*vm.Spec) {
	for _, spec := range specs {
		st := s.loadDesktopState(spec.Name)
		if st == nil {
			continue
		}
		if _, err := s.ensureDesktopForward(spec.Name, st.HostPort, st.Public); err != nil {
			log.Printf("desktop %s: restore listener on %d: %v", spec.Name, st.HostPort, err)
		}
	}
}

// removeDesktopMaps drops desktop port maps left by older versions.
func (s *Server) removeDesktopMaps(ctx context.Context, spec *vm.Spec, push func(context.Context, int) error) error {
	cl := s.Clusters.Get(spec.ClusterID)
	if cl == nil {
		return nil
	}
	found := false
	for _, m := range cl.Maps {
		if m.VM == spec.Name && m.GuestPort == desktopWebPort {
			found = true
		}
	}
	if !found {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, mapPushTimeout)
	defer cancel()
	if err := s.mapMu.Lock(ctx); err != nil {
		return err
	}
	defer s.mapMu.Unlock()
	for _, m := range s.Clusters.Get(spec.ClusterID).Maps {
		if m.VM == spec.Name && m.GuestPort == desktopWebPort {
			s.relays.Remove(m.Proto, m.HostPort)
			if _, err := s.Clusters.RemoveMap(spec.ClusterID, m.HostPort, m.Proto); err != nil {
				return err
			}
		}
	}
	return push(ctx, spec.ClusterID)
}

func (s *Server) handleDesktopStop(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	spec, err := s.getSpec(name)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	s.deskMu.Lock()
	defer s.deskMu.Unlock()
	if st, err := s.instance(spec).Running(); err == nil && st != nil {
		_, _ = s.sshRun(spec, fmt.Sprintf("vncserver -kill %s >/dev/null 2>&1; fuser -k %d/tcp >/dev/null 2>&1 || true", desktopVNCDisplay, desktopWebPort))
	}
	s.stopDesktopForward(name)
	if err := s.removeDesktopMaps(r.Context(), spec, s.pushGatewayConfigCtx); err != nil {
		writeErr(w, 500, err)
		return
	}
	_ = os.Remove(s.desktopPath(name))
	writeJSON(w, 200, map[string]any{"stopped": name})
}

func desktopURL(st *desktopState, name string) string {
	host := "127.0.0.1"
	if st.Public {
		host = "0.0.0.0"
	}
	// noVNC accepts fragment settings; fragments are not sent in HTTP
	// requests. The URL remains secret in browser history and terminal output.
	// resize=remote sizes the guest display to the window instead of
	// scaling a fixed framebuffer; quality and compression favour latency
	// on a local link
	fragment := url.Values{"password": {st.Password}, "autoconnect": {"true"}, "resize": {"remote"}, "quality": {"6"}, "compression": {"2"}, "vm": {name}}
	return fmt.Sprintf("http://%s:%d/vnc.html#%s", host, st.HostPort, fragment.Encode())
}

// The bracket trick keeps pgrep from matching the shell running the check.
func (s *Server) desktopSessionRunning(spec *vm.Spec) bool {
	out, err := s.sshRun(spec, fmt.Sprintf(
		"pgrep -f '[X]tigervnc %s' >/dev/null && pgrep -f '[w]ebsockify -D' >/dev/null && echo UP", desktopVNCDisplay))
	return err == nil && strings.Contains(out, "UP")
}

func (s *Server) startDesktopSession(spec *vm.Spec, st *desktopState) error {
	xstartup := `#!/bin/sh
unset SESSION_MANAGER
unset DBUS_SESSION_BUS_ADDRESS
exec startxfce4
`
	script := fmt.Sprintf(`set -e
mkdir -p "$HOME/.vnc" "$HOME/.config"
cat > "$HOME/.vnc/xstartup" <<'EOF'
%sEOF
chmod +x "$HOME/.vnc/xstartup"
printf '%%s' %s | vncpasswd -f > "$HOME/.vnc/passwd"
chmod 600 "$HOME/.vnc/passwd"
vncserver -kill %s >/dev/null 2>&1 || true
fuser -k %d/tcp >/dev/null 2>&1 || true
vncserver %s -geometry %s -depth 24 -FrameRate 60 -SecurityTypes VncAuth -localhost yes
for i in $(seq 1 15); do fuser %d/tcp >/dev/null 2>&1 || break; sleep 1; done
nohup websockify -D --web /usr/share/novnc %d 127.0.0.1:%d >/dev/null 2>&1
`, xstartup, shellQuote(st.Password), desktopVNCDisplay, desktopWebPort, desktopVNCDisplay, st.Size, desktopWebPort, desktopWebPort, desktopVNCPort)
	if out, err := s.sshRun(spec, script); err != nil {
		return fmt.Errorf("start desktop session: %v\n%s", err, out)
	}
	for i := 0; i < 30; i++ {
		out, err := s.sshRun(spec, fmt.Sprintf("curl -s -o /dev/null -w %%{http_code} http://127.0.0.1:%d/vnc.html", desktopWebPort))
		if err == nil && strings.TrimSpace(out) == "200" {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("novnc did not become ready on :%d within 60s", desktopWebPort)
}
