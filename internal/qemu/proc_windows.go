//go:build windows

package qemu

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/huangzheng2016/pvm/internal/fsutil"
	"golang.org/x/sys/windows"
)

const stillActiveExitCode = 259

func IdentityOf(pid int) (*ProcIdentity, error) {
	if pid <= 0 {
		return nil, errNoProc
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return nil, errNoProc
		}
		return nil, err
	}
	defer windows.CloseHandle(h)

	var exitCode uint32
	if err := windows.GetExitCodeProcess(h, &exitCode); err != nil {
		return nil, err
	}
	if exitCode != stillActiveExitCode {
		return nil, errNoProc
	}

	buf := make([]uint16, windows.MAX_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return nil, err
	}
	comm := filepath.Base(windows.UTF16ToString(buf[:size]))

	var creation, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &creation, &exit, &kernel, &user); err != nil {
		return nil, err
	}
	started := strconv.FormatInt(creation.Nanoseconds(), 10)
	return &ProcIdentity{PID: pid, Started: started, Comm: comm, State: "R"}, nil
}

func verifyProcessIdentity(st *RunState) (bool, error) {
	if st.Ident == nil || st.PID <= 0 || st.PID != st.Ident.PID {
		return false, fmt.Errorf("refusing to signal invalid process identity")
	}
	cur, err := IdentityOf(st.PID)
	if errors.Is(err, errNoProc) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("cannot verify process %d before signalling: %w", st.PID, err)
	}
	if startedWithoutState(cur.Started) != startedWithoutState(st.Ident.Started) || !strings.EqualFold(cur.Comm, st.Ident.Comm) {
		return false, nil
	}
	return true, nil
}

func terminateGraceful(st *RunState) error {
	return terminateWindows(st)
}

func terminateForce(st *RunState) error {
	return terminateWindows(st)
}

func terminateWindows(st *RunState) error {
	match, err := verifyProcessIdentity(st)
	if err != nil || !match {
		return err
	}
	proc, err := os.FindProcess(st.PID)
	if err == nil {
		if killErr := proc.Kill(); killErr == nil {
			return nil
		}
	}
	match, err = verifyProcessIdentity(st)
	if err != nil || !match {
		return err
	}
	cmd := exec.Command("taskkill", "/PID", strconv.Itoa(st.PID), "/T", "/F")
	fsutil.ConfigureHiddenCmd(cmd)
	if out, taskErr := cmd.CombinedOutput(); taskErr != nil {
		if stillMatch, _ := verifyProcessIdentity(st); stillMatch {
			return fmt.Errorf("taskkill %d: %s: %w", st.PID, strings.TrimSpace(string(out)), taskErr)
		}
	}
	return nil
}
