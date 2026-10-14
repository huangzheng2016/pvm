package qemu

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// ProcIdentity prevents a recycled PID from being signalled by mistake.
type ProcIdentity struct {
	PID     int    `json:"pid"`
	Started string `json:"started"` // ps lstart, opaque but stable per boot+pid
	Comm    string `json:"comm"`
	State   string `json:"state"`
}

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

// Only a definitive not-found means dead; probe failures read as alive so
// callers never kill or double-start on uncertainty.
func (id *ProcIdentity) Alive() bool {
	cur, err := IdentityOf(id.PID)
	if err != nil {
		return err != errNoProc
	}
	if cur.State != "" && strings.HasPrefix(cur.State, "Z") {
		return false
	}
	return startedWithoutState(cur.Started) == startedWithoutState(id.Started) && cur.Comm == id.Comm
}

func startedWithoutState(started string) string {
	fields := strings.Fields(started)
	if len(fields) > 1 && len(fields[len(fields)-1]) <= 3 {
		return strings.Join(fields[:len(fields)-1], " ")
	}
	return started
}

type errT string

func (e errT) Error() string { return string(e) }

const errNoProc = errT("process not found")
