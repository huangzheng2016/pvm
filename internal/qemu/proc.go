package qemu

import (
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
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=,lstart=").Output()
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
func (id *ProcIdentity) Alive() bool {
	cur, err := IdentityOf(id.PID)
	if err != nil {
		return false
	}
	return cur.Started == id.Started && strings.Contains(cur.Comm, "qemu")
}

type errT string

func (e errT) Error() string { return string(e) }

const errNoProc = errT("process not found")
