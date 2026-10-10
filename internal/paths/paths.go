// Package paths defines the ~/.pvm data layout.
package paths

import (
	"os"
	"path/filepath"
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
	// The share dir is exported into guests via 9p security_model=none:
	// guests see host mode bits as-is, so it must be world-traversable
	// for non-root guest users.
	if err := os.MkdirAll(p.Share, 0o755); err != nil {
		return err
	}
	return os.Chmod(p.Share, 0o755)
}

func (p *Paths) ConfigFile() string       { return filepath.Join(p.Home, "config.yaml") }
func (p *Paths) DaemonSock() string       { return filepath.Join(p.Run, "daemon.sock") }
func (p *Paths) DaemonLock() string       { return filepath.Join(p.Run, "daemon.lock") }
func (p *Paths) DaemonStopMark() string   { return filepath.Join(p.Run, "daemon.stopped") }
func (p *Paths) DaemonLog() string        { return filepath.Join(p.Run, "daemon.log") }
func (p *Paths) SSHKey() string           { return filepath.Join(p.Keys, "id_ed25519") }
func (p *Paths) SSHPubKey() string        { return p.SSHKey() + ".pub" }
func (p *Paths) KnownHosts() string       { return filepath.Join(p.Keys, "known_hosts") }
func (p *Paths) VMDir(name string) string { return filepath.Join(p.VMs, name) }
