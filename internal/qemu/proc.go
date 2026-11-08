//go:build !windows

package qemu

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func IdentityOf(pid int) (*ProcIdentity, error) {
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=,lstart=,stat=")
	// lstart is locale-sensitive (LC_TIME); CJK dates break the field-count parse.
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(bytes.TrimSpace(out)) == 0 {
			return nil, errNoProc
		}
		return nil, err
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return nil, errNoProc
	}
	fields := strings.Fields(line)
	if len(fields) < 6 {
		return nil, errNoProc
	}
	// comm has no spaces for qemu-system-*; lstart is the trailing 5 fields.
	comm := fields[0]
	started := strings.Join(fields[1:6], " ")
	state := ""
	if len(fields) > 6 {
		state = fields[6]
	}
	return &ProcIdentity{PID: pid, Started: started, Comm: comm, State: state}, nil
}

// killVerified re-checks the recorded identity immediately before
// signalling, so a recycled PID is never signalled by mistake.
func killVerified(st *RunState, sig syscall.Signal) error {
	if st.Ident == nil || st.PID <= 0 || st.PID != st.Ident.PID {
		return fmt.Errorf("refusing to signal invalid process identity")
	}
	cur, err := IdentityOf(st.PID)
	if errors.Is(err, errNoProc) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot verify process %d before signalling: %w", st.PID, err)
	}
	if startedWithoutState(cur.Started) != startedWithoutState(st.Ident.Started) || cur.Comm != st.Ident.Comm {
		return nil
	}
	if err := syscall.Kill(st.PID, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill(%d, %v): %w", st.PID, sig, err)
	}
	return nil
}

func terminateGraceful(st *RunState) error {
	return killVerified(st, syscall.SIGTERM)
}

func terminateForce(st *RunState) error {
	return killVerified(st, syscall.SIGKILL)
}
