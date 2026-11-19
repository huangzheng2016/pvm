package daemon

import (
	"bufio"
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/agent"
	"github.com/huangzheng2016/pvm/internal/apps"
	"github.com/huangzheng2016/pvm/internal/vm"
)

func TestAppCatalogRoute(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Server{}).handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/apps/catalog", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != len(apps.List()) || len(list) == 0 {
		t.Fatalf("catalog has %d entries, want %d", len(list), len(apps.List()))
	}
	for _, e := range list {
		id, _ := e["id"].(string)
		if !appIDRe.MatchString(id) {
			t.Errorf("catalog id %q is not an app id", id)
		}
		var keys []string
		for k := range e {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if want := []string{"description", "homepage", "id", "name", "source", "start", "window"}; !slices.Equal(keys, want) {
			t.Errorf("%s: keys = %v, want %v", id, keys, want)
		}
	}
}

func TestCatalogAppFor(t *testing.T) {
	for prog, want := range map[string]string{"claude-desktop": "claude-desktop", "/usr/bin/claude": "claude-code", "codex": "codex", "gedit": ""} {
		if id, ok := catalogAppFor(prog); id != want || ok != (want != "") {
			t.Errorf("catalogAppFor(%q) = %q, %v; want %q", prog, id, ok, want)
		}
	}
}

// newAppInstallServer has dev1 running with the real guest agent server on
// its agent socket, so install scripts run on this machine, and a catalog
// of test scripts in place of the built-in one.
func newAppInstallServer(t *testing.T, catalog map[string]apps.App) (*Server, *vm.Spec) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs bash and a unix socket")
	}
	s, dev1, _ := newMapTestServer(t)
	markAuditVMRunning(t, s, dev1)
	ln, err := net.Listen("unix", s.P.AgentSock("dev1"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ln.Close()
		s.agentMu.Lock()
		defer s.agentMu.Unlock()
		for _, c := range s.agents {
			c.Close()
		}
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go agent.NewServer().Serve(conn)
		}
	}()
	old := catalogApp
	catalogApp = func(id string) (apps.App, bool) {
		a, ok := catalog[id]
		a.ID = id
		return a, ok
	}
	t.Cleanup(func() { catalogApp = old })
	return s, dev1
}

func postAppInstall(t *testing.T, s *Server, vmName, body string) (int, []appInstallFrame, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	s.handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/vms/"+vmName+"/apps/install", strings.NewReader(body)))
	if rec.Code != 200 {
		return rec.Code, nil, rec.Body.String()
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/x-ndjson" {
		t.Errorf("Content-Type = %q", ct)
	}
	var frames []appInstallFrame
	sc := bufio.NewScanner(rec.Body)
	for sc.Scan() {
		var f appInstallFrame
		if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
			t.Fatalf("frame %q: %v", sc.Text(), err)
		}
		frames = append(frames, f)
	}
	return rec.Code, frames, ""
}

func installedApps(t *testing.T, s *Server, vmName string) []string {
	t.Helper()
	st, err := s.loadAppState(vmName)
	if err != nil {
		t.Fatal(err)
	}
	return st.Installed
}

func TestAppInstall(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "ran")
	s, _ := newAppInstallServer(t, map[string]apps.App{
		"zed":   {Script: "echo \"user $PVM_USER\"\necho warning >&2\n"},
		"alpha": {Script: "cat >/dev/null\necho alpha\n"},
		"bad":   {Script: "echo before\nexit 7\necho after\n"},
		"never": {Script: "touch '" + marker + "'\n"},
		"win":   {Window: true, Script: "touch '" + marker + "'\n"},
	})

	code, frames, _ := postAppInstall(t, s, "dev1", `{"ids":["zed","bad","never"]}`)
	if code != 200 || len(frames) != 7 {
		t.Fatalf("status %d, frames %+v", code, frames)
	}
	if !reflect.DeepEqual(frames[0], appInstallFrame{App: "zed", State: "installing"}) || !reflect.DeepEqual(frames[3], appInstallFrame{App: "zed", State: "installed"}) {
		t.Errorf("zed state frames: %+v, %+v", frames[0], frames[3])
	}
	// stdout and stderr are separate streams, so their order is not fixed
	out := []string{frames[1].Out, frames[2].Out}
	slices.Sort(out)
	if !slices.Equal(out, []string{"user pvm", "warning"}) || frames[1].App != "zed" || frames[2].App != "zed" {
		t.Errorf("zed output frames: %+v, %+v", frames[1], frames[2])
	}
	if !reflect.DeepEqual(frames[4], appInstallFrame{App: "bad", State: "installing"}) || !reflect.DeepEqual(frames[5], appInstallFrame{App: "bad", Out: "before"}) {
		t.Errorf("bad frames: %+v, %+v", frames[4], frames[5])
	}
	last := frames[6]
	if !last.Done || !slices.Equal(last.Installed, []string{"zed"}) || last.Error != "install bad in dev1: the install script failed with exit status 7; not attempted: never" {
		t.Errorf("last frame = %+v", last)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the app after the failed one was installed")
	}
	if got := installedApps(t, s, "dev1"); !slices.Equal(got, []string{"zed"}) {
		t.Errorf("installed after a failed run = %v", got)
	}

	// a repeat and a second app: recorded once, sorted
	code, frames, _ = postAppInstall(t, s, "dev1", `{"ids":["zed","alpha","zed"]}`)
	if code != 200 || len(frames) == 0 {
		t.Fatalf("status %d, frames %+v", code, frames)
	}
	if last := frames[len(frames)-1]; !last.Done || last.Error != "" || !slices.Equal(last.Installed, []string{"zed", "alpha"}) {
		t.Errorf("last frame = %+v", last)
	}
	if got := installedApps(t, s, "dev1"); !slices.Equal(got, []string{"alpha", "zed"}) {
		t.Errorf("installed = %v", got)
	}

	// Xpra comes first for a window app; the test cluster has no gateway
	// to reach the VM through, so it fails and the script does not run
	code, frames, _ = postAppInstall(t, s, "dev1", `{"ids":["win"]}`)
	if code != 200 || len(frames) != 2 {
		t.Fatalf("status %d, frames %+v", code, frames)
	}
	if last := frames[1]; !last.Done || len(last.Installed) != 0 || !strings.HasPrefix(last.Error, "install win in dev1: install xpra in dev1: ") {
		t.Errorf("last frame = %+v", last)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the script ran without Xpra")
	}

	for _, tc := range []struct {
		name, vm, body string
		code           int
		want           string
	}{
		{"unknown id", "dev1", `{"ids":["zed","nope"]}`, 400, `unknown app \"nope\"`},
		{"no ids", "dev1", `{"ids":[]}`, 400, "at least one app id"},
		{"bad body", "dev1", `[`, 400, "bad request"},
		{"stopped vm", "dev2", `{"ids":["zed"]}`, 409, "not running"},
		{"unknown vm", "gone", `{"ids":["zed"]}`, 404, "gone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, _, body := postAppInstall(t, s, tc.vm, tc.body)
			if code != tc.code || !strings.Contains(body, tc.want) {
				t.Errorf("status %d, body %s; want %d with %q", code, body, tc.code, tc.want)
			}
		})
	}
	if got := installedApps(t, s, "dev1"); !slices.Equal(got, []string{"alpha", "zed"}) {
		t.Errorf("installed after rejected requests = %v", got)
	}

	for _, path := range []string{"/v1/vms/dev1/apps", "/v1/apps"} {
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if got := strings.TrimSpace(rec.Body.String()); rec.Code != 200 || got != `{"apps":[],"installed":{"dev1":["alpha","zed"]}}` {
			t.Errorf("GET %s = %d %s", path, rec.Code, got)
		}
	}
}

func TestAppInstallTimeout(t *testing.T) {
	s, _ := newAppInstallServer(t, map[string]apps.App{"slow": {Script: "echo started\nsleep 60\n"}})
	old := appInstallTimeout
	appInstallTimeout = 2 * time.Second
	t.Cleanup(func() { appInstallTimeout = old })
	start := time.Now()
	code, frames, _ := postAppInstall(t, s, "dev1", `{"ids":["slow"]}`)
	if code != 200 || len(frames) == 0 {
		t.Fatalf("status %d, frames %+v", code, frames)
	}
	if last := frames[len(frames)-1]; !last.Done || last.Error != "install slow in dev1: timed out after 2s" {
		t.Errorf("last frame = %+v", last)
	}
	if time.Since(start) > 30*time.Second {
		t.Errorf("the timeout took %s", time.Since(start))
	}
	if got := installedApps(t, s, "dev1"); len(got) != 0 {
		t.Errorf("installed after a timeout = %v", got)
	}
}

// A running install script must not keep the app list waiting.
func TestAppInstallLeavesAppListFree(t *testing.T) {
	dir := t.TempDir()
	started, release := filepath.Join(dir, "started"), filepath.Join(dir, "release")
	s, _ := newAppInstallServer(t, map[string]apps.App{
		"slow": {Script: "touch '" + started + "'\nwhile [ ! -e '" + release + "' ]; do sleep 0.1; done\n"},
	})
	done := make(chan []appInstallFrame, 1)
	go func() {
		_, frames, _ := postAppInstall(t, s, "dev1", `{"ids":["slow"]}`)
		done <- frames
	}()
	t.Cleanup(func() {
		os.WriteFile(release, nil, 0o600)
		<-done
	})
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the install script did not start")
		}
	}
	listed := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		s.handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/apps", nil))
		listed <- rec.Code
	}()
	select {
	case code := <-listed:
		if code != 200 {
			t.Errorf("app list = %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the app list waits for the install")
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	frames := <-done
	done <- frames
	if len(frames) == 0 || !reflect.DeepEqual(frames[len(frames)-1], appInstallFrame{Done: true, Installed: []string{"slow"}}) {
		t.Errorf("frames = %+v", frames)
	}
	if got := installedApps(t, s, "dev1"); !slices.Equal(got, []string{"slow"}) {
		t.Errorf("installed = %v", got)
	}
}
