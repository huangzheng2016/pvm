//go:build windows

package agent

import "os/exec"

func configureExecProcessGroup(cmd *exec.Cmd) {}

func killExecProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
