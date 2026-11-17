package cli

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detachClient keeps the client out of the terminal's console and process
// group, so closing the terminal leaves the windows open.
func detachClient(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS}
}
