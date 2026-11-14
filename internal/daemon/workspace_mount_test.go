package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/workspace"
)

type fakeMounts struct {
	mu       sync.Mutex
	list     []*workspace.Workspace
	up       bool
	fail     error
	sessions atomic.Int32
	active   atomic.Int32
}

func (f *fakeMounts) supervisor() *mountSupervisor {
	return &mountSupervisor{
		list: func() ([]*workspace.Workspace, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return append([]*workspace.Workspace(nil), f.list...), nil
		},
		vmUp: func(string) bool {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.up
		},
		session: func(ctx context.Context, ws *workspace.Workspace, mounted func()) error {
			f.sessions.Add(1)
			f.mu.Lock()
			fail := f.fail
			f.mu.Unlock()
			if fail != nil {
				return fail
			}
			f.active.Add(1)
			defer f.active.Add(-1)
			mounted()
			<-ctx.Done()
			return ctx.Err()
		},
		backoffMin: 5 * time.Millisecond,
		backoffMax: 20 * time.Millisecond,
	}
}

func (f *fakeMounts) set(fn func()) {
	f.mu.Lock()
	fn()
	f.mu.Unlock()
}

func sftpWorkspace(id string) *workspace.Workspace {
	return &workspace.Workspace{ID: id, VM: "dev1", Mode: workspace.ModeMount, Transport: workspace.TransportSFTP, HostPath: "/src/" + id, GuestPath: workspace.GuestPathFor(id)}
}

// startSupervisor runs m with a tick long enough that tests drive
// reconciles themselves.
func startSupervisor(t *testing.T, m *mountSupervisor) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.run(ctx, time.Hour)
		close(done)
	}()
	waitFor(t, 5*time.Second, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.ctx != nil
	})
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func waitState(t *testing.T, m *mountSupervisor, id, state string) mountStatus {
	t.Helper()
	var st mountStatus
	waitFor(t, 5*time.Second, func() bool { st = m.get(id); return st.State == state })
	return st
}

func TestMountSupervisorFollowsVMAndMetadata(t *testing.T) {
	f := &fakeMounts{list: []*workspace.Workspace{
		sftpWorkspace("app"),
		{ID: "nine", VM: "dev1", Mode: workspace.ModeMount, Transport: workspace.Transport9p},
		{ID: "synced", VM: "dev1", Mode: workspace.ModeSync},
	}}
	m := f.supervisor()
	startSupervisor(t, m)
	m.reconcile()
	if st := m.get("app"); st.State != mountStopped || st.Since == "" {
		t.Fatalf("VM down: %+v", st)
	}
	for _, id := range []string{"nine", "synced", "missing"} {
		if st := m.get(id); st.State != mountAbsent {
			t.Errorf("%s: %+v, want absent", id, st)
		}
	}

	f.set(func() { f.up = true })
	m.reconcile()
	st := waitState(t, m, "app", mountMounted)
	if _, err := time.Parse(time.RFC3339, st.Since); err != nil {
		t.Errorf("since %q: %v", st.Since, err)
	}
	m.reconcile()
	if n := f.sessions.Load(); n != 1 {
		t.Fatalf("sessions = %d after a no-op reconcile", n)
	}

	f.set(func() { f.up = false })
	m.reconcile()
	waitState(t, m, "app", mountStopped)
	waitFor(t, 5*time.Second, func() bool { return f.active.Load() == 0 })

	f.set(func() { f.up = true })
	m.reconcile()
	waitState(t, m, "app", mountMounted)

	// A changed binding restarts the session.
	f.set(func() { f.list[0] = sftpWorkspace("app"); f.list[0].ReadOnly = true })
	m.reconcile()
	waitState(t, m, "app", mountMounted)
	waitFor(t, 5*time.Second, func() bool { return f.sessions.Load() == 3 && f.active.Load() == 1 })

	f.set(func() { f.list = nil })
	m.reconcile()
	waitState(t, m, "app", mountAbsent)
	waitFor(t, 5*time.Second, func() bool { return f.active.Load() == 0 })
}

func TestMountSupervisorRetriesWithBackoff(t *testing.T) {
	f := &fakeMounts{list: []*workspace.Workspace{sftpWorkspace("app")}, up: true, fail: errors.New("ssh: connection refused")}
	m := f.supervisor()
	startSupervisor(t, m)
	m.reconcile()
	waitFor(t, 5*time.Second, func() bool { return f.sessions.Load() >= 4 })
	st := m.get("app")
	if st.State != mountError && st.State != mountConnecting {
		t.Fatalf("state = %+v", st)
	}
	st = waitState(t, m, "app", mountError)
	if !strings.Contains(st.Error, "connection refused") {
		t.Fatalf("error = %q", st.Error)
	}
	f.set(func() { f.fail = nil })
	waitState(t, m, "app", mountMounted)
}

func TestMountSupervisorHoldUntilStart(t *testing.T) {
	f := &fakeMounts{list: []*workspace.Workspace{sftpWorkspace("app")}, up: true}
	m := f.supervisor()
	startSupervisor(t, m)
	m.reconcile()
	waitState(t, m, "app", mountMounted)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.stop(ctx, "app"); err != nil {
		t.Fatal(err)
	}
	if n := f.active.Load(); n != 0 {
		t.Fatalf("stop returned with %d active sessions", n)
	}
	m.reconcile()
	if st := m.get("app"); st.State != mountStopped {
		t.Fatalf("held workspace restarted: %+v", st)
	}
	if st := m.start("app"); st.State != mountConnecting && st.State != mountMounted {
		t.Fatalf("start = %+v", st)
	}
	waitState(t, m, "app", mountMounted)
	if n := f.sessions.Load(); n != 2 {
		t.Fatalf("sessions = %d", n)
	}
}

func TestMountSupervisorStopsOnShutdown(t *testing.T) {
	f := &fakeMounts{list: []*workspace.Workspace{sftpWorkspace("app")}, up: true}
	m := f.supervisor()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.run(ctx, time.Hour)
		close(done)
	}()
	waitState(t, m, "app", mountMounted)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return")
	}
	if n := f.active.Load(); n != 0 {
		t.Fatalf("%d sessions left after shutdown", n)
	}
	m.reconcile()
	if n := f.sessions.Load(); n != 1 {
		t.Fatalf("reconcile after shutdown started a session (%d)", n)
	}
}

func TestWorkspaceMountRoutes(t *testing.T) {
	s := &Server{P: paths.New(t.TempDir())}
	h := s.handler()
	do := func(method, path string) (int, map[string]any) {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(method, path, nil))
		var body map[string]any
		json.Unmarshal(rr.Body.Bytes(), &body)
		return rr.Code, body
	}
	for _, method := range []string{"GET", "POST", "DELETE"} {
		if code, _ := do(method, "/v1/workspaces/Bad_ID/mount"); code != 400 {
			t.Errorf("%s invalid id = %d, want 400", method, code)
		}
	}
	if code, body := do("GET", "/v1/workspaces/app/mount"); code != 200 || body["state"] != mountAbsent {
		t.Errorf("GET unknown = %d %v", code, body)
	}
	if code, _ := do("POST", "/v1/workspaces/app/mount"); code != 404 {
		t.Errorf("POST unknown = %d, want 404", code)
	}
	if code, body := do("DELETE", "/v1/workspaces/app/mount"); code != 200 || body["state"] != mountAbsent {
		t.Errorf("DELETE unknown = %d %v", code, body)
	}
}
