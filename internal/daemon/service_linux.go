//go:build linux

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/huangzheng2016/pvm/internal/fsutil"
	"github.com/huangzheng2016/pvm/internal/paths"
)

// ServiceInstall writes a systemd user unit and enables it.
func ServiceInstall(p *paths.Paths) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	unit := fmt.Sprintf(`[Unit]
Description=pvm daemon

[Service]
ExecStart=%s daemon run
Restart=on-failure

[Install]
WantedBy=default.target
`, exe)
	path := filepath.Join(dir, "pvm.service")
	if err := fsutil.AtomicWrite(path, []byte(unit), 0o644); err != nil {
		return err
	}
	if out, err := exec.Command("systemctl", "--user", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("daemon-reload: %v\n%s", err, out)
	}
	if out, err := exec.Command("systemctl", "--user", "enable", "--now", "pvm.service").CombinedOutput(); err != nil {
		return fmt.Errorf("enable: %v\n%s", err, out)
	}
	return nil
}

func ServiceUninstall(p *paths.Paths) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	exec.Command("systemctl", "--user", "disable", "--now", "pvm.service").Run()
	return os.Remove(filepath.Join(home, ".config", "systemd", "user", "pvm.service"))
}
