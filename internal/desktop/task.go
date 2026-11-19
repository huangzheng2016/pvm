package desktop

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	eventTask   = "task"
	eventOutput = "task-output"

	stepPending = "pending"
	stepRunning = "running"
	stepDone    = "done"
	stepFailed  = "failed"
)

// step is one command of a task.
type step struct {
	// argv are the arguments to pvm. A step without argv only waits.
	argv []string
	// dir is the working directory, for commands that read the workspace
	// from it.
	dir string
	// display is the command line shown to the user and offered for
	// copying, with secrets left out. It may differ from argv where the
	// form a person would type differs.
	display string
	// secrets are cut from every output line.
	secrets []string
	// wait names a VM whose stages are followed until it is ready, after
	// argv succeeded.
	wait string
}

// Step is the frontend's view of a step.
type Step struct {
	Command string `json:"command"`
	State   string `json:"state"` // pending, running, done or failed
}

// Task is the frontend's view of a user action: sent as the "task" event
// on every change and returned when the action ends.
type Task struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	Steps []Step `json:"steps"`
	Done  bool   `json:"done"`
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	// Remaining are the commands after a failed step, to run by hand.
	Remaining []string `json:"remaining"`
}

// TaskOutput is the "task-output" event: one line a step printed.
type TaskOutput struct {
	ID   int64  `json:"id"`
	Step int    `json:"step"`
	Line string `json:"line"`
}

func (a *App) send(event string, data any) {
	if a.emit != nil {
		a.emit(event, data)
	}
}

// runTask runs the steps in order and stops at the first that fails. It
// never undoes a finished step: what exists after a failure is left for
// the user, with the commands that were not run.
func (a *App) runTask(title string, steps []step) Task {
	t := Task{ID: a.taskID.Add(1), Title: title, Steps: make([]Step, len(steps)), Remaining: []string{}}
	a.runningMu.Lock()
	a.running[t.ID] = title
	a.runningMu.Unlock()
	defer func() {
		a.runningMu.Lock()
		delete(a.running, t.ID)
		a.runningMu.Unlock()
	}()
	for i, s := range steps {
		t.Steps[i] = Step{Command: s.display, State: stepPending}
	}
	publish := func() {
		snapshot := t
		snapshot.Steps = append([]Step(nil), t.Steps...)
		a.send(eventTask, snapshot)
	}
	for i, s := range steps {
		t.Steps[i].State = stepRunning
		publish()
		if err := a.runStep(t.ID, i, s); err != nil {
			t.Steps[i].State = stepFailed
			t.Done, t.Error = true, err.Error()
			for _, rest := range steps[i+1:] {
				t.Remaining = append(t.Remaining, rest.display)
			}
			publish()
			return t
		}
		t.Steps[i].State = stepDone
	}
	t.Done, t.OK = true, true
	publish()
	return t
}

func (a *App) runStep(id int64, index int, s step) error {
	var mu sync.Mutex
	last := ""
	line := func(text string) {
		for _, secret := range s.secrets {
			text = strings.ReplaceAll(text, secret, "***")
		}
		mu.Lock()
		defer mu.Unlock()
		if strings.TrimSpace(text) != "" {
			last = strings.TrimSpace(text)
		}
		a.send(eventOutput, TaskOutput{ID: id, Step: index, Line: text})
	}
	if len(s.argv) > 0 {
		code, err := a.run.run(command{argv: s.argv, dir: s.dir}, line)
		if err != nil {
			return err
		}
		if code != 0 {
			mu.Lock()
			defer mu.Unlock()
			if last != "" {
				return fmt.Errorf("%s (exit status %d)", last, code)
			}
			return fmt.Errorf("%s failed with exit status %d", s.display, code)
		}
	}
	if s.wait != "" {
		return a.follow(s.wait, line)
	}
	return nil
}

// follow polls the VM list until the VM is ready or failed, printing each
// new stage. The wait is bounded like the CLI's own, and ends early when
// the list stays unreadable: the app has no ctrl-c.
func (a *App) follow(name string, line func(string)) error {
	deadline := time.Now().Add(a.followTimeout)
	shown := ""
	var lostAt time.Time
	for {
		in, err := a.findVM(name)
		if errors.As(err, &errNoVM{}) {
			return err
		}
		if err != nil {
			if lostAt.IsZero() {
				lostAt = time.Now()
				line("cannot read the vm list, trying again: " + err.Error())
			}
			if time.Since(lostAt) >= a.lostTimeout {
				return fmt.Errorf("stopped waiting for %s, the vm list cannot be read: %w; once the daemon runs, `pvm start %s` waits for it again", name, err, name)
			}
		} else {
			lostAt = time.Time{}
			state, stage, hint := vmState(in)
			switch state {
			case stateReady:
				line(name + ": ready")
				return nil
			case stateFailed:
				return fmt.Errorf("%s failed: %s", name, hint)
			case stateStopped:
				return fmt.Errorf("%s stopped before it was ready; see `pvm logs %s`", name, name)
			}
			if text := stateStarting + " (" + stage + ")"; stage != "" && text != shown {
				shown = text
				line(name + ": " + text)
			}
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s was not ready after %s; watch it with `pvm logs %s --follow`", name, a.followTimeout, name)
		}
		time.Sleep(a.pollInterval)
	}
}

// errNoVM stops a wait at once: polling cannot make a VM appear.
type errNoVM struct{ name string }

func (e errNoVM) Error() string { return "vm " + e.name + " does not exist" }

func (a *App) findVM(name string) (vmInfo, error) {
	infos, err := a.readVMs()
	if err != nil {
		return vmInfo{}, err
	}
	for _, in := range infos {
		if in.Spec.Name == name && in.Spec.Role != "gateway" {
			return in, nil
		}
	}
	return vmInfo{}, errNoVM{name}
}
