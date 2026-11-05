package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"

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
		Home:     home,
		Images:   filepath.Join(home, "images"),
		VMs:      filepath.Join(home, "vms"),
		Clusters: filepath.Join(home, "clusters"),
		Share:    filepath.Join(home, "share"),
		Keys:     filepath.Join(home, "keys"),
		Run:      filepath.Join(home, "run"),
		Runtime:  filepath.Join(home, "runtime"),
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
func (p *Paths) AgentSock(name string) string {
	return filepath.Join(p.VMDir(name), "agent.sock")
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
	if stat, ok := info.Sys().(*syscall.Stat_t); ok && stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("pvm state directory is owned by another user; restore ownership before continuing")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("pvm state directory is accessible to other users; restrict its permissions to 0700 before continuing")
	}
	return nil
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
