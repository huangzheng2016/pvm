package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/fsutil"
	"github.com/huangzheng2016/pvm/internal/vm"
)

const (
	desktopVNCDisplay = ":0"
	desktopVNCPort    = 5900 // 5900 + display number
	desktopWebPort    = 6080 // websockify/noVNC listen port inside the guest
)

var desktopSizeRe = regexp.MustCompile(`^[1-9]\d{2,3}x[1-9]\d{2,3}$`)

// desktopState is the per-VM desktop record. It is persisted under the VM
// dir (0600) so a daemon restart can keep issuing URLs for a live VNC
// session instead of killing it just because the password was forgotten.
type desktopState struct {
	Password string `json:"password"`
	HostPort int    `json:"host_port"`
	Size     string `json:"size"`
}

type desktopResponse struct {
	URL      string `json:"url"`
	HostPort int    `json:"host_port"`
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

// handleDesktop idempotently starts a native XFCE desktop (TigerVNC +
// noVNC) inside the VM as the login user and publishes it through a
// cluster port map. The returned URL carries the VNC password as a noVNC
// query parameter over loopback, so no login page and no daemon-side
// credential proxy are needed.
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
	if vm.IsAlpine(spec) {
		writeErr(w, 400, fmt.Errorf("desktop is not supported on alpine (no golden image); use ubuntu/debian"))
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

	if out, err := s.sshRun(spec, "command -v vncserver websockify"); err != nil || strings.Count(strings.TrimSpace(out), "/") < 2 {
		writeErr(w, 400, fmt.Errorf("desktop packages missing in %s; create the vm from a golden image built with desktop support (pvm image build)", name))
		return
	}

	st := s.loadDesktopState(name)
	running := s.desktopSessionRunning(spec)

	if running && st != nil {
		writeJSON(w, 200, desktopResponse{URL: desktopURL(st), HostPort: st.HostPort})
		return
	}
	// Session down or state lost: (re)create. A lost state means we cannot
	// recover the old password, so the session is restarted fresh.
	if running {
		_, _ = s.sshRun(spec, fmt.Sprintf("vncserver -kill %s >/dev/null 2>&1; pkill -f 'websockify.*:%d' >/dev/null 2>&1", desktopVNCDisplay, desktopWebPort))
	}

	st = &desktopState{Password: randHex(8), Size: size}
	if err := s.startDesktopSession(spec, st); err != nil {
		writeErr(w, 500, err)
		return
	}

	// publish websockify through the cluster map (reuse an existing one)
	hostPort := 0
	for _, m := range s.Clusters.Get(spec.ClusterID).Maps {
		if m.VM == spec.Name && m.GuestPort == desktopWebPort && m.Proto == "tcp" {
			hostPort = m.HostPort
		}
	}
	if hostPort == 0 {
		port, err := freeLoopbackPort("tcp")
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		m, err := s.Clusters.AddMap(spec.ClusterID, spec.Name, spec.IP, port, desktopWebPort, "tcp")
		if err != nil {
			writeErr(w, 409, err)
			return
		}
		if err := s.relays.Ensure("tcp", m.HostPort, m.PoolPort); err != nil {
			s.Clusters.RemoveMap(spec.ClusterID, m.HostPort, "tcp")
			writeErr(w, 500, err)
			return
		}
		if err := s.pushGatewayConfig(spec.ClusterID); err != nil {
			writeErr(w, 500, err)
			return
		}
		hostPort = m.HostPort
	}
	st.HostPort = hostPort
	if err := s.saveDesktopState(name, st); err != nil {
		writeErr(w, 500, fmt.Errorf("save desktop state: %v", err))
		return
	}
	writeJSON(w, 200, desktopResponse{URL: desktopURL(st), HostPort: st.HostPort})
}

// handleDesktopStop kills the VNC session and websockify in the guest and
// removes the host port map.
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
		_, _ = s.sshRun(spec, fmt.Sprintf("vncserver -kill %s >/dev/null 2>&1; pkill -f 'websockify.*:%d' >/dev/null 2>&1", desktopVNCDisplay, desktopWebPort))
	}
	if cl := s.Clusters.Get(spec.ClusterID); cl != nil {
		for _, m := range cl.Maps {
			if m.VM == spec.Name && m.GuestPort == desktopWebPort {
				s.relays.Remove(m.Proto, m.HostPort)
				s.Clusters.RemoveMap(spec.ClusterID, m.HostPort, m.Proto)
			}
		}
		_ = s.pushGatewayConfig(spec.ClusterID)
	}
	_ = os.Remove(s.desktopPath(name))
	writeJSON(w, 200, map[string]any{"stopped": name})
}

func desktopURL(st *desktopState) string {
	return fmt.Sprintf("http://127.0.0.1:%d/vnc.html?password=%s&autoconnect=true&resize=scale", st.HostPort, st.Password)
}

// desktopSessionRunning reports whether Xtigervnc for our display and a
// websockify for our web port are both alive in the guest.
func (s *Server) desktopSessionRunning(spec *vm.Spec) bool {
	out, err := s.sshRun(spec, fmt.Sprintf(
		"pgrep -f 'Xtigervnc.*%s' >/dev/null && pgrep -f 'websockify.*:%d' >/dev/null && echo UP", desktopVNCDisplay, desktopWebPort))
	return err == nil && strings.Contains(out, "UP")
}

// startDesktopSession writes the xstartup and password files as the login
// user, starts Xtigervnc on the display and websockify/noVNC on the web
// port, then waits for vnc.html to answer.
func (s *Server) startDesktopSession(spec *vm.Spec, st *desktopState) error {
	xstartup := `#!/bin/sh
unset SESSION_MANAGER
unset DBUS_SESSION_BUS_ADDRESS
exec startxfce4
`
	script := fmt.Sprintf(`set -e
mkdir -p "$HOME/.vnc"
cat > "$HOME/.vnc/xstartup" <<'EOF'
%sEOF
chmod +x "$HOME/.vnc/xstartup"
printf '%%s' %s | vncpasswd -f > "$HOME/.vnc/passwd"
chmod 600 "$HOME/.vnc/passwd"
vncserver -kill %s >/dev/null 2>&1 || true
pkill -f 'websockify.*:%d' >/dev/null 2>&1 || true
vncserver %s -geometry %s -depth 24 -SecurityTypes VncAuth -localhost yes
nohup websockify -D --web /usr/share/novnc %d 127.0.0.1:%d >/dev/null 2>&1
`, xstartup, shellQuote(st.Password), desktopVNCDisplay, desktopWebPort, desktopVNCDisplay, st.Size, desktopWebPort, desktopVNCPort)
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
