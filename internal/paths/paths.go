package paths

import (
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/huangzheng2016/pvm/internal/native"
)

type Paths struct {
	Home     string
	Images   string
	VMs      string
	Clusters string
	Share    string
	Keys     string
	Run      string
	Runtime  string
	// Workspaces holds workspace metadata; created on first use.
	Workspaces string
}

func Default() (*Paths, error) {
	if env := os.Getenv("PVM_HOME"); env != "" {
		return New(env), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return New(filepath.Join(home, ".pvm")), nil
}

func New(home string) *Paths {
	return &Paths{
		Home:       home,
		Images:     filepath.Join(home, "images"),
		VMs:        filepath.Join(home, "vms"),
		Clusters:   filepath.Join(home, "clusters"),
		Share:      filepath.Join(home, "share"),
		Keys:       filepath.Join(home, "keys"),
		Run:        filepath.Join(home, "run"),
		Runtime:    filepath.Join(home, "runtime"),
		Workspaces: filepath.Join(home, "workspaces"),
	}
}

func (p *Paths) Ensure() error {
	for _, d := range []string{p.Home, p.Images, p.VMs, p.Clusters, p.Keys, p.Run, p.Runtime} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	// Guests see host mode bits through 9p. Create a traversable global
	// share, but do not rewrite permissions of an existing user directory.
	if err := os.MkdirAll(p.Share, 0o755); err != nil {
		return err
	}
	return nil
}

func (p *Paths) ConfigFile() string       { return filepath.Join(p.Home, "config.yaml") }
func (p *Paths) DaemonSock() string       { return filepath.Join(p.Run, "daemon.sock") }
func (p *Paths) DaemonAddr() string       { return filepath.Join(p.Run, "daemon.addr") }
func (p *Paths) DaemonAdminToken() string { return filepath.Join(p.Run, "admin.token") }
func (p *Paths) DaemonLock() string       { return filepath.Join(p.Run, "daemon.lock") }
func (p *Paths) DaemonRunLock() string    { return filepath.Join(p.Run, "daemon.run.lock") }
func (p *Paths) DaemonStopMark() string   { return filepath.Join(p.Run, "daemon.stopped") }
func (p *Paths) DaemonLog() string        { return filepath.Join(p.Run, "daemon.log") }
func (p *Paths) SSHKey() string           { return filepath.Join(p.Keys, "id_ed25519") }
func (p *Paths) SSHPubKey() string        { return p.SSHKey() + ".pub" }
func (p *Paths) KnownHosts() string       { return filepath.Join(p.Keys, "known_hosts") }
func (p *Paths) VMDir(name string) string { return filepath.Join(p.VMs, name) }
func (p *Paths) MountDir(name string) string {
	return filepath.Join(p.VMDir(name), "mounts")
}
func (p *Paths) MountSlot(name string, slot int) string {
	return filepath.Join(p.MountDir(name), fmt.Sprintf("slot%d", slot))
}
func (p *Paths) QMPSock(name string) string {
	return SocketEndpoint(filepath.Join(p.VMDir(name), "qmp.sock"))
}
func (p *Paths) SerialSock(name string) string {
	return SocketEndpoint(filepath.Join(p.VMDir(name), "serial.sock"))
}
func (p *Paths) AgentSock(name string) string {
	return SocketEndpoint(filepath.Join(p.VMDir(name), "agent.sock"))
}

// StreamSock is the host end of the pvm.stream virtio-serial port.
func (p *Paths) StreamSock(name string) string {
	return SocketEndpoint(filepath.Join(p.VMDir(name), "stream.sock"))
}

// SocketEndpoint returns sockPath on Unix, or a stable loopback TCP endpoint
// (tcp:127.0.0.1:<port>) on Windows where QEMU chardev/QMP/stream sockets
// use inet sockets.
func SocketEndpoint(sockPath string) string {
	if runtime.GOOS != "windows" || strings.HasPrefix(sockPath, "tcp:") {
		return sockPath
	}
	if addr := storedEndpoint(sockPath); addr != "" {
		return addr
	}
	return allocEndpoint(sockPath)
}

func storedEndpoint(sockPath string) string {
	b, err := os.ReadFile(sockPath + ".tcp")
	if err != nil {
		return ""
	}
	if addr := strings.TrimSpace(string(b)); strings.HasPrefix(addr, "tcp:127.0.0.1:") {
		return addr
	}
	return ""
}

// RefreshSocketEndpoint picks a new port for sockPath when the stored one
// can no longer be bound, and reports whether it did. Windows reserves
// port ranges anew at boot, which can cover a port stored earlier; QEMU
// then fails to start. Call it only while nothing listens on the port,
// that is before starting the QEMU that will.
func RefreshSocketEndpoint(sockPath string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	return refreshEndpoint(sockPath)
}

func refreshEndpoint(sockPath string) bool {
	addr := storedEndpoint(sockPath)
	if addr == "" {
		return false
	}
	if ln, err := net.Listen("tcp", strings.TrimPrefix(addr, "tcp:")); err == nil {
		ln.Close()
		return false
	}
	os.Remove(sockPath + ".tcp")
	allocEndpoint(sockPath)
	return true
}

// RefreshVMSockets refreshes the endpoints QEMU listens on for a VM that
// is not running, and reports whether any changed.
func (p *Paths) RefreshVMSockets(name string) bool {
	changed := false
	for _, s := range []string{"qmp.sock", "serial.sock", "agent.sock", "stream.sock"} {
		if RefreshSocketEndpoint(filepath.Join(p.VMDir(name), s)) {
			changed = true
		}
	}
	return changed
}

func allocEndpoint(sockPath string) string {
	tcpFile := sockPath + ".tcp"
	h := fnv.New32a()
	_, _ = h.Write([]byte(filepath.Clean(sockPath)))
	basePort := 40000 + int(h.Sum32()%15000)
	port := basePort
	if ln, err := net.Listen("tcp", "127.0.0.1:0"); err == nil {
		port = ln.Addr().(*net.TCPAddr).Port
		_ = ln.Close()
	} else {
		for i := 0; i < 64; i++ {
			cand := basePort + i
			ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", cand))
			if err == nil {
				_ = ln.Close()
				port = cand
				break
			}
		}
	}
	addr := fmt.Sprintf("tcp:127.0.0.1:%d", port)
	if fi, err := os.Stat(filepath.Dir(sockPath)); err == nil && fi.IsDir() {
		_ = os.WriteFile(tcpFile, []byte(addr+"\n"), 0o600)
	}
	return addr
}

func (p *Paths) AgentBin() string {
	return filepath.Join(p.Runtime, native.GuestBinary("agent"))
}

func (p *Paths) VMSSHKey(name string) string { return filepath.Join(p.Keys, "vms", name, "id_ed25519") }
func (p *Paths) VMShare(name string) string  { return filepath.Join(p.VMDir(name), "share") }

func checkPrivateDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("pvm state directory must be a directory, not a symlink or file")
	}
	return checkPrivateDirOS(info)
}

// CheckPermissions diagnoses existing state directories without changing user-selected paths.
func (p *Paths) CheckPermissions() error {
	for _, d := range []string{p.Home, p.Images, p.VMs, p.Clusters, p.Keys, p.Run, p.Runtime} {
		if err := checkPrivateDir(d); err != nil {
			return err
		}
	}
	return nil
}
