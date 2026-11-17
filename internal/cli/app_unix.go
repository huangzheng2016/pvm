//go:build !windows

package cli

import (
	"os/exec"
	"syscall"
)

// detachClient starts the client in its own session, so closing the
// terminal leaves the windows open.
func detachClient(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
