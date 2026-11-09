package cli

import "fmt"

func prepareDaemonForRuntimeChange(goos string, daemonRunning, anyRunning, noRestart, qemuChange bool, stop func() int) error {
	if goos != "windows" || !daemonRunning {
		return nil
	}
	if noRestart {
		if qemuChange && anyRunning {
			return fmt.Errorf("cannot replace QEMU while VMs are running on Windows; omit --no-restart or stop the daemon first")
		}
		return nil
	}
	if stop() != 0 {
		return fmt.Errorf("could not stop the daemon before replacing pvm and QEMU; retry the command or restore with `pvm upgrade --rollback -y`")
	}
	return nil
}
