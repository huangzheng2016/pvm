package qemu

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/huangzheng2016/pvm/internal/fsutil"
)

type Instance struct {
	Cfg       VMConfig
	StatePath string
	Toolchain *Toolchain

	// mu serializes operations within this Instance value only. The daemon
	// creates a fresh Instance per call, so cross-call lifecycle exclusion
	// must use LockVM; mu is not a daemon-wide or per-VM lock.
	mu  sync.Mutex
	cmd *exec.Cmd
}

type RunState struct {
	PID     int           `json:"pid"`
	Ident   *ProcIdentity `json:"ident"`
	Args    []string      `json:"args"`
	Accel   Accel         `json:"accel"`
	Started time.Time     `json:"started"`
}

var keyedLocks sync.Map

func LockVM(name string) *sync.Mutex {
	v, _ := keyedLocks.LoadOrStore(name, &sync.Mutex{})
	return v.(*sync.Mutex)
}

func (in *Instance) loadState() (*RunState, error) {
	data, err := os.ReadFile(in.StatePath)
	if err != nil {
		return nil, err
	}
	var st RunState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// Corrupted state must remain an error so callers cannot mistake it for
// "not running" and start a second QEMU.
func (in *Instance) Running() (*RunState, error) {
	st, err := in.loadState()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("vm state %s: %w", in.StatePath, err)
	}
	if st.PID <= 0 || st.Ident == nil || st.Ident.PID != st.PID || st.Ident.Comm == "" || st.Ident.Started == "" {
		return nil, fmt.Errorf("vm state %s: invalid process identity", in.StatePath)
	}
	if !st.Ident.Alive() {
		return nil, nil
	}
	return st, nil
}

type errEarlyExit struct{ error }

func (e errEarlyExit) Unwrap() error { return e.error }

func (in *Instance) Start() error {
	in.mu.Lock()
	defer in.mu.Unlock()
	st, err := in.Running()
	if err != nil {
		return fmt.Errorf("cannot start vm safely: %w; move the corrupted state file aside or delete it after confirming no qemu process is running", err)
	}
	if st != nil {
		return fmt.Errorf("vm already running (pid %d)", st.PID)
	}
	if err := in.startWithAccel(in.Cfg.Accel); err == nil {
		return nil
	} else {
		fb, ok := in.Cfg.Accel.Fallback()
		var ee errEarlyExit
		if !ok || !errors.As(err, &ee) {
			return err
		}
		log.Printf("qemu: accel %s failed, falling back to %s: %s%s", in.Cfg.Accel, fb, firstLine(err.Error()), FallbackHint(err))
		in.Cfg.Accel = fb
		return in.startWithAccel(fb)
	}
}

func (in *Instance) startWithAccel(accel Accel) error {
	cfg := in.Cfg
	cfg.Accel = accel
	args, err := BuildArgs(in.Toolchain, &cfg)
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(cfg.ConsoleLog+".qemu", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(in.Toolchain.System, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	in.cmd = cmd

	// Early-exit detection: accelerator or argv errors kill qemu within a
	// few hundred ms and the message lands in the log.
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		tail, _ := os.ReadFile(cfg.ConsoleLog + ".qemu")
		return errEarlyExit{fmt.Errorf("qemu exited early: %v\n%s", err, lastLines(tail, 20))}
	case <-time.After(800 * time.Millisecond):
	}

	ident, err := IdentityOf(cmd.Process.Pid)
	if err != nil {
		cmd.Process.Kill()
		tail, _ := os.ReadFile(cfg.ConsoleLog + ".qemu")
		console, _ := os.ReadFile(cfg.ConsoleLog)
		return errEarlyExit{fmt.Errorf("qemu died after launch: %v\nqemu log:\n%s\nconsole:\n%s", err, lastLines(tail, 15), lastLines(console, 15))}
	}
	st := RunState{PID: cmd.Process.Pid, Ident: ident, Args: append([]string{in.Toolchain.System}, args...), Accel: accel, Started: time.Now()}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		cmd.Process.Kill()
		return err
	}
	if err := fsutil.AtomicWrite(in.StatePath, data, 0o600); err != nil {
		cmd.Process.Kill()
		<-exited
		return err
	}
	go func() {
		<-exited
		// A concurrent Start may already have replaced the state files
		// with a new generation; only remove files still owned by this one.
		in.mu.Lock()
		defer in.mu.Unlock()
		cur, err := in.loadState()
		if err != nil || cur.PID != st.PID || !cur.Started.Equal(st.Started) {
			return
		}
		os.Remove(in.StatePath)
		os.Remove(cfg.QMPSock)
		os.Remove(cfg.SerialSock)
	}()
	return nil
}

// Stop does graceful powerdown via QMP, escalating to SIGTERM/SIGKILL.
func (in *Instance) Stop() error {
	in.mu.Lock()
	defer in.mu.Unlock()
	st, err := in.Running()
	if err != nil {
		return fmt.Errorf("cannot stop vm safely: %w", err)
	}
	if st == nil {
		os.Remove(in.StatePath)
		return nil
	}
	if q, err := DialQMP(in.Cfg.QMPSock); err == nil {
		q.Powerdown()
		q.Close()
		if waitDead(st.Ident, 30*time.Second) {
			return nil
		}
	}
	if err := killVerified(st, syscall.SIGTERM); err != nil {
		return err
	}
	if waitDead(st.Ident, 10*time.Second) {
		return nil
	}
	if err := killVerified(st, syscall.SIGKILL); err != nil {
		return err
	}
	// discard=unmap hole punching can keep a killed process in disk io for minutes.
	if !waitDead(st.Ident, 2*time.Minute) && st.Ident.Alive() {
		return fmt.Errorf("process %d still alive after SIGKILL", st.PID)
	}
	return nil
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

func waitDead(id *ProcIdentity, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if !id.Alive() {
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	return !id.Alive()
}

func lastLines(b []byte, n int) string {
	lines := bytes.Split(bytes.TrimSpace(b), []byte("\n"))
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return string(bytes.Join(lines, []byte("\n")))
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
