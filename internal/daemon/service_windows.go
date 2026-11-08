//go:build windows

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/huangzheng2016/pvm/internal/fsutil"
	"github.com/huangzheng2016/pvm/internal/paths"
)

const windowsTaskName = "pvm-daemon"

func ServiceInstall(p *paths.Paths) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, _ = filepath.EvalSymlinks(exe)
	tr := fmt.Sprintf(`"%s" daemon run`, exe)
	cmd := exec.Command("schtasks", "/Create", "/F", "/SC", "ONLOGON", "/TN", windowsTaskName, "/TR", tr)
	fsutil.ConfigureHiddenCmd(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("schtasks create: %v\n%s", err, out)
	}
	runCmd := exec.Command("schtasks", "/Run", "/TN", windowsTaskName)
	fsutil.ConfigureHiddenCmd(runCmd)
	_ = runCmd.Run()
	return nil
}

func ServiceUninstall(p *paths.Paths) error {
	cmd := exec.Command("schtasks", "/Delete", "/F", "/TN", windowsTaskName)
	fsutil.ConfigureHiddenCmd(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("schtasks delete: %v\n%s", err, out)
	}
	return nil
}
