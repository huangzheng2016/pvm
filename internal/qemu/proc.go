package qemu

import (
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
}

func IdentityOf(pid int) (*ProcIdentity, error) {
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=,lstart=")
	// lstart is locale-sensitive (LC_TIME); CJK dates break the field-count parse.
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

// Only a definitive not-found means dead; probe failures read as alive so
// callers never kill or double-start on uncertainty.
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
