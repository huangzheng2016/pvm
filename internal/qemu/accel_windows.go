//go:build windows

package qemu

import (
	"os/exec"
	"strings"

	"github.com/huangzheng2016/pvm/internal/fsutil"
)

func windowsHypervisorPlatformEnabled() bool {
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", "(Get-WindowsOptionalFeature -Online -FeatureName HypervisorPlatform).State")
	fsutil.ConfigureHiddenCmd(cmd)
	out, err := cmd.Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "Enabled")
}
