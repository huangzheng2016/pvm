package qemu

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// ProcIdentity pins down a process beyond its PID so a recycled PID is
// never signalled by mistake (same defence as the platform supervisor).
type ProcIdentity struct {
	PID     int    `json:"pid"`
	Started string `json:"started"` // ps lstart, opaque but stable per boot+pid
	Comm    string `json:"comm"`
}

func IdentityOf(pid int) (*ProcIdentity, error) {
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=,lstart=")
	// ps renders lstart in the caller's locale (LC_TIME); a CJK date has
	// fewer whitespace fields and breaks the field-count parse, which used
	// to kill healthy qemus as "process not found".
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
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
	started := strings.Join(fields[len(fields)-5:], " ")
	return &ProcIdentity{PID: pid, Started: started, Comm: comm}, nil
}

// Alive reports whether a process matching this identity still exists.
// Comparison is pid + exact start time + exact comm: macOS truncates
// comm to 16 bytes so a "contains qemu" test would fail, but a recycled
// PID can never share both start time and comm. Only a definitive
// not-found means dead; a transient probe failure must not convince the
// caller to kill or double-start the process, so those read as alive.
func (id *ProcIdentity) Alive() bool {
	cur, err := IdentityOf(id.PID)
	if err != nil {
		return err != errNoProc
	}
	return cur.Started == id.Started && cur.Comm == id.Comm
}

type errT string

func (e errT) Error() string { return string(e) }

const errNoProc = errT("process not found")
