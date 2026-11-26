//go:build darwin || linux

package keepawake

import (
	"fmt"
	"os/exec"
	"time"
)

func acquire() (func(), error) {
	var cmd *exec.Cmd
	if isDarwin() {
		// -i prevents idle system sleep only; display sleep and lid sleep still work.
		cmd = exec.Command("/usr/bin/caffeinate", "-i")
	} else {
		if _, err := exec.LookPath("systemd-inhibit"); err != nil {
			return nil, err
		}
		cmd = exec.Command("systemd-inhibit", "--what=sleep", "--mode=block", "--no-ask-password", "--why=pvm virtual machines running", "sleep", "infinity")
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return nil, fmt.Errorf("keepawake helper exited early: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	var once bool
	return func() {
		if once {
			return
		}
		once = true
		_ = cmd.Process.Kill()
		<-done
	}, nil
}
