package qemu

import "strings"

// ProcIdentity prevents a recycled PID from being signalled by mistake.
type ProcIdentity struct {
	PID     int    `json:"pid"`
	Started string `json:"started"` // ps lstart or creation timestamp, opaque but stable per boot+pid
	Comm    string `json:"comm"`
	State   string `json:"state"`
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
