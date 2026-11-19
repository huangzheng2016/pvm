package desktop

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/config"
	"github.com/huangzheng2016/pvm/internal/exit"
	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/workspace"
)

// fakeDaemon answers reads from JSON kept per path.
type fakeDaemon struct {
	mu      sync.Mutex
	healthy bool
	routes  map[string]string
	// onGet runs before each read, to change the answers between polls.
	onGet func(path string)
}

func (d *fakeDaemon) Healthy() bool { return d.healthy }

func (d *fakeDaemon) set(path, body string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.routes[path] = body
}

func (d *fakeDaemon) Get(path string, out any) error {
	if d.onGet != nil {
		d.onGet(path)
	}
	d.mu.Lock()
	body, ok := d.routes[path]
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("no route %s", path)
	}
	return json.Unmarshal([]byte(body), out)
}

// fakeRunner records every pvm command and fails those picked by fail.
type fakeRunner struct {
	mu    sync.Mutex
	calls []command
	fail  func(c command) bool
	// during runs while the command "runs".
	during func(c command, line func(string))
}

func (r *fakeRunner) run(c command, line func(string)) (int, error) {
	r.mu.Lock()
	r.calls = append(r.calls, c)
	r.mu.Unlock()
	if r.during != nil {
		r.during(c, line)
	}
	if r.fail != nil && r.fail(c) {
		line("pvm: it broke")
		return 1, nil
	}
	return 0, nil
}

func (r *fakeRunner) argvs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.calls {
		out = append(out, strings.Join(c.argv, " "))
	}
	return out
}

type fixture struct {
	app    *App
	daemon *fakeDaemon
	runner *fakeRunner

	mu        sync.Mutex
	events    []any
	terminals []string
	folders   []string
	atLogin   *bool
}

const (
	userVMs  = `[{"spec":{"name":"dev1","image":"ubuntu:24.04","role":"user","cpus":2,"mem_mb":2048,"disk_gb":20},"running":true,"ready":true,"stage":{"stage":"ready"}}]`
	catalog  = `[{"id":"claude-desktop","name":"Claude Desktop","window":true,"start":"claude-desktop"},{"id":"codex","name":"Codex CLI","start":"codex"}]`
	clusters = `[{"id":1,"gateway":"pvm-gw1"}]`
)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	p := paths.New(t.TempDir())
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	f := &fixture{
		daemon: &fakeDaemon{healthy: true, routes: map[string]string{
			"/v1/vms":          userVMs,
			"/v1/clusters":     clusters,
			"/v1/apps/catalog": catalog,
			"/v1/status":       `{"version":"1.2.3","accel":"whpx","tcg_fallback":1}`,
		}},
		runner: &fakeRunner{},
	}
	f.app = New(p, Options{
		Emit: func(event string, data any) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.events = append(f.events, map[string]any{"event": event, "data": data})
		},
		PickFolder: func() (string, error) { return "picked", nil },
	})
	f.app.daemon = f.daemon
	f.app.run = f.runner
	f.app.backend = func() (string, error) { return "native", nil }
	f.app.pollInterval = time.Millisecond
	f.app.followTimeout = 2 * time.Second
	f.app.lostTimeout = 2 * time.Second
	f.app.sys = system{
		openTerminal: func(dir string, args []string) error {
			f.terminals = append(f.terminals, dir+"|"+strings.Join(args, " "))
			return nil
		},
		openFolder:    func(path string) error { f.folders = append(f.folders, path); return nil },
		appAtLogin:    func() (bool, error) { return f.atLogin != nil && *f.atLogin, nil },
		setAppAtLogin: func(on bool) error { f.atLogin = &on; return nil },
		daemonAtLogin: func() (bool, error) { return true, nil },
	}
	return f
}

func (f *fixture) saveExit(t *testing.T, e *exit.Entry) {
	t.Helper()
	if err := exit.SaveEntry(f.app.exitsDir(), e); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) saveWorkspace(t *testing.T, id, vm string) string {
	t.Helper()
	host := t.TempDir()
	ws := &workspace.Workspace{ID: id, VM: vm, Mode: workspace.ModeSync, HostPath: host,
		GuestPath: workspace.GuestPathFor(id), LastTransfer: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	if err := workspace.NewStore(f.app.p).Save(ws); err != nil {
		t.Fatal(err)
	}
	return host
}

func TestVMState(t *testing.T) {
	type stage = struct {
		Stage  string `json:"stage"`
		Detail string `json:"detail"`
	}
	for _, tc := range []struct {
		name             string
		in               vmInfo
		state, stg, hint string
	}{
		{"stopped", vmInfo{}, stateStopped, "", ""},
		{"stopped keeps no stage", vmInfo{Stage: stage{Stage: "failed", Detail: "x"}}, stateStopped, "", ""},
		{"ready", vmInfo{Running: true, Ready: true, Stage: stage{Stage: "ready"}}, stateReady, "", ""},
		{"no stage yet", vmInfo{Running: true}, stateStarting, "", ""},
		{"boot", vmInfo{Running: true, Stage: stage{Stage: "boot"}}, stateStarting, "boot", ""},
		{"provision", vmInfo{Running: true, Stage: stage{Stage: "provision", Detail: "cloud-init"}}, stateStarting, "provision", ""},
		{"identity reset pending", vmInfo{Running: true, Stage: stage{Stage: "ready"}}, stateStarting, "", ""},
		{"failed", vmInfo{Running: true, Stage: stage{Stage: "failed", Detail: "see pvm logs dev1"}}, stateFailed, "", "see pvm logs dev1"},
	} {
		state, stg, hint := vmState(tc.in)
		if state != tc.state || stg != tc.stg || hint != tc.hint {
			t.Errorf("%s: got %q %q %q, want %q %q %q", tc.name, state, stg, hint, tc.state, tc.stg, tc.hint)
		}
	}
}

func TestState(t *testing.T) {
	f := newFixture(t)
	st, err := f.app.State()
	if err != nil {
		t.Fatal(err)
	}
	if !st.DaemonHealthy || st.DaemonStopped || st.DaemonVersion != "1.2.3" || st.Accel != "whpx" || st.TCGFallback != 1 || st.DataDir != f.app.p.Home {
		t.Fatalf("state = %+v", st)
	}

	f.daemon.healthy = false
	if err := os.WriteFile(f.app.p.DaemonStopMark(), []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err = f.app.State()
	if err != nil {
		t.Fatal(err)
	}
	if st.DaemonHealthy || !st.DaemonStopped || st.DaemonVersion != "" {
		t.Fatalf("stopped state = %+v", st)
	}
}

func TestReads(t *testing.T) {
	f := newFixture(t)
	f.daemon.set("/v1/vms", `[
		{"spec":{"name":"pvm-gw1","role":"gateway"},"running":true},
		{"spec":{"name":"dev2","role":"user","cpus":4,"mem_mb":4096,"disk_gb":40},"running":true,"stage":{"stage":"failed","detail":"cloud-init failed; pvm logs dev2"}},
		{"spec":{"name":"dev1","role":"user"},"running":true,"stage":{"stage":"ssh"}},
		{"spec":{"name":"dev3","role":"user"}}]`)
	f.daemon.set("/v1/clusters", `[{"id":1,"gateway":"pvm-gw1","default_exit":"my-exit",
		"vm_exits":{"dev2":"direct","gone":"my-exit"},
		"rules":[{"type":"domain-suffix","pattern":"example.com","action":"proxy","exit":"my-exit"}]}]`)
	if err := config.SetDefaultVM(f.app.p.ConfigFile(), "dev3"); err != nil {
		t.Fatal(err)
	}
	f.saveExit(t, &exit.Entry{Name: "my-exit", Endpoint: "vps.example.com:51820", AutoTZ: "Europe/Berlin", Locale: "de_DE.UTF-8"})
	f.saveExit(t, &exit.Entry{Name: "my-warp", Provider: exit.ProviderWARP, TZ: "Asia/Tokyo"})

	vms, err := f.app.VMs()
	if err != nil {
		t.Fatal(err)
	}
	want := []VM{
		{Name: "dev1", State: stateStarting, Stage: "ssh", Exit: "my-exit"},
		{Name: "dev2", State: stateFailed, Hint: "cloud-init failed; pvm logs dev2", CPUs: 4, MemMB: 4096, DiskGB: 40, Exit: "direct", ExitOwn: true},
		{Name: "dev3", State: stateStopped, Exit: "my-exit", Default: true},
	}
	if !reflect.DeepEqual(vms, want) {
		t.Fatalf("vms = %+v\nwant  %+v", vms, want)
	}

	exits, err := f.app.Exits()
	if err != nil {
		t.Fatal(err)
	}
	wantExits := []Exit{
		{Name: "my-exit", Kind: "agent", Endpoint: "vps.example.com:51820", TZ: "Europe/Berlin(auto)", Locale: "de_DE.UTF-8",
			Default: true, VMs: []string{"dev1", "dev3", "gone"}, Rules: []string{"domain-suffix example.com"}},
		{Name: "my-warp", Kind: "warp", TZ: "Asia/Tokyo", VMs: []string{}, Rules: []string{}},
	}
	if !reflect.DeepEqual(exits, wantExits) {
		t.Fatalf("exits = %+v\nwant   %+v", exits, wantExits)
	}

	host := f.saveWorkspace(t, "web", "dev1")
	wss, err := f.app.Workspaces()
	if err != nil {
		t.Fatal(err)
	}
	wantWS := []Workspace{{ID: "web", VM: "dev1", Mode: "sync", HostPath: host, GuestPath: "/workspace/web", LastTransfer: "2026-01-02T03:04:05Z"}}
	if !reflect.DeepEqual(wss, wantWS) {
		t.Fatalf("workspaces = %+v", wss)
	}

	settings, err := f.app.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if settings != (Settings{DefaultVM: "dev3", DaemonAtLogin: true}) {
		t.Fatalf("settings = %+v", settings)
	}

	f.daemon.set("/v1/vms/dev1/apps", `{"apps":[],"installed":{"dev1":["codex"]}}`)
	installed, err := f.app.InstalledApps("dev1")
	if err != nil {
		t.Fatal(err)
	}
	if len(installed) != 1 || installed[0].ID != "codex" || installed[0].Window {
		t.Fatalf("installed = %+v", installed)
	}

	images, err := f.app.Images()
	if err != nil {
		t.Fatal(err)
	}
	if len(images) == 0 || images[0] != (Image{Spec: "ubuntu:26.04"}) {
		t.Fatalf("images = %+v", images)
	}

	defaults, err := f.app.CreateDefaults()
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	if defaults != (CreateDefaults{User: "pvm", CPUs: cfg.Defaults.CPUs, MemMB: cfg.Defaults.MemMB, DiskGB: cfg.Defaults.DiskGB, Exit: "my-exit"}) {
		t.Fatalf("create defaults = %+v", defaults)
	}
}

// TestActionArgv pins the exact pvm command of every action.
func TestActionArgv(t *testing.T) {
	f := newFixture(t)
	f.saveExit(t, &exit.Entry{Name: "my-exit"})
	host := f.saveWorkspace(t, "web", "dev1")
	loose := f.saveWorkspace(t, "loose", "")
	app := f.app

	task := func(fn func() (Task, error)) func() error {
		return func() error {
			tk, err := fn()
			if err == nil && !tk.OK {
				err = errors.New(tk.Error)
			}
			return err
		}
	}
	for _, tc := range []struct {
		name string
		do   func() error
		want []string
		dir  string
	}{
		{"daemon start", task(app.StartDaemon), []string{"daemon start"}, ""},
		{"start", task(func() (Task, error) { return app.StartVM("dev1") }), []string{"start dev1 --no-wait"}, ""},
		{"stop", task(func() (Task, error) { return app.StopVM("dev1") }), []string{"stop dev1"}, ""},
		{"delete", task(func() (Task, error) { return app.DeleteVM("dev1", true) }), []string{"rm dev1 -y"}, ""},
		{"app command", task(func() (Task, error) { return app.RunApp("dev1", " gedit  notes.txt ") }), []string{"app dev1 -- gedit notes.txt"}, ""},
		{"app flag-like command", task(func() (Task, error) { return app.RunApp("dev1", "--no-attach") }), []string{"app dev1 -- --no-attach"}, ""},
		{"app url", task(func() (Task, error) { return app.RunApp("dev1", "https://example.com/a?b=c") }), []string{"app dev1 https://example.com/a?b=c"}, ""},
		{"app not a url", task(func() (Task, error) { return app.RunApp("dev1", "ftp://example.com") }), []string{"app dev1 -- ftp://example.com"}, ""},
		{"catalog window app", task(func() (Task, error) { return app.RunCatalogApp("dev1", "claude-desktop") }), []string{"app dev1 -- claude-desktop"}, ""},
		{"files", task(func() (Task, error) { return app.OpenFiles("dev1") }), []string{"app dev1 -- thunar"}, ""},
		{"app settings", task(func() (Task, error) {
			return app.ConfigureApps("dev1", AppConfig{Clipboard: "both", Drop: "on", Audio: "off"})
		}), []string{"app config dev1 --clipboard both --drop on --audio off"}, ""},
		{"app settings, one", task(func() (Task, error) { return app.ConfigureApps("dev1", AppConfig{Drop: "off"}) }), []string{"app config dev1 --drop off"}, ""},
		{"install", task(func() (Task, error) { return app.InstallApps("dev1", []string{"codex", "claude-desktop"}) }), []string{"app install dev1 codex claude-desktop"}, ""},
		{"default vm", task(func() (Task, error) { return app.SetDefaultVM("dev1") }), []string{"default-vm dev1"}, ""},
		{"clear default vm", task(app.ClearDefaultVM), []string{"default-vm --clear"}, ""},
		{"vm exit", task(func() (Task, error) { return app.SetVMExit("dev1", "my-exit", true) }), []string{"route dev1 --via my-exit"}, ""},
		{"vm direct", task(func() (Task, error) { return app.SetVMExit("dev1", "direct", true) }), []string{"route dev1 --via direct"}, ""},
		{"default exit", task(func() (Task, error) { return app.SetDefaultExit("my-exit") }), []string{"route default --via my-exit"}, ""},
		{"add warp", task(func() (Task, error) { return app.AddWarpExit("my-warp") }), []string{"exit add-warp my-warp"}, ""},
		{"test exit", task(func() (Task, error) { return app.TestExit("my-exit", false) }), []string{"exit test my-exit"}, ""},
		{"speed test", task(func() (Task, error) { return app.TestExit("my-exit", true) }), []string{"exit test my-exit --speed"}, ""},
		{"remove exit", task(func() (Task, error) { return app.RemoveExit("my-exit", true) }), []string{"exit rm my-exit"}, ""},
		{"workspace app", task(func() (Task, error) { return app.RunWorkspaceApp("web", "code .") }), []string{"app dev1 -- code ."}, host},
		{"workspace app, no vm", task(func() (Task, error) { return app.RunWorkspaceApp("loose", "code .") }), []string{"app -- code ."}, loose},
		{"workspace files", task(func() (Task, error) { return app.OpenWorkspaceFiles("web") }), []string{"app dev1 -- thunar /workspace/web"}, host},
		{"push", task(func() (Task, error) { return app.SyncWorkspace("web", "push") }), []string{"workspace push web"}, ""},
		{"pull", task(func() (Task, error) { return app.SyncWorkspace("web", "pull") }), []string{"workspace pull web"}, ""},
		{"create workspace", task(func() (Task, error) {
			return app.CreateWorkspace(CreateWorkspaceRequest{ID: "api", VM: "dev1", Mode: "mount", HostPath: host, ReadOnly: true})
		}), []string{"workspace create api dev1 --mode mount --path " + host + " --read-only"}, ""},
		{"create workspace, confirmed", task(func() (Task, error) {
			return app.CreateWorkspace(CreateWorkspaceRequest{ID: "api", VM: "dev1", Mode: "copy", HostPath: host, Confirmed: true})
		}), []string{"workspace create api dev1 --mode copy --path " + host + " -y"}, ""},
		{"remove workspace", task(func() (Task, error) { return app.RemoveWorkspace("web", true) }), []string{"workspace rm web -y"}, ""},
		{"daemon at login", task(func() (Task, error) { return app.SetDaemonAtLogin(true) }), []string{"daemon install"}, ""},
		{"no daemon at login", task(func() (Task, error) { return app.SetDaemonAtLogin(false) }), []string{"daemon uninstall"}, ""},
		{"create, default exit", task(func() (Task, error) {
			return app.CreateVM(CreateRequest{Name: "dev9", Image: "debian:13", User: "pvm", CPUs: 2, MemMB: 2048, DiskGB: 20})
		}), []string{
			"create --name dev9 --image debian:13 --user pvm --cpus 2 --mem 2048 --disk 20 --no-start",
			"start dev9 --no-wait",
		}, ""},
	} {
		f.runner.calls = nil
		if tc.name == "create, default exit" {
			f.daemon.set("/v1/vms", strings.ReplaceAll(userVMs, "dev1", "dev9"))
		}
		if err := tc.do(); err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if got := f.runner.argvs(); !slices.Equal(got, tc.want) {
			t.Errorf("%s: ran %q, want %q", tc.name, got, tc.want)
		}
		if dir := f.runner.calls[0].dir; dir != tc.dir {
			t.Errorf("%s: dir %q, want %q", tc.name, dir, tc.dir)
		}
	}

	// actions that open a window instead of running a task
	f.runner.calls = nil
	for _, err := range []error{
		app.OpenTerminal("dev1"),
		app.OpenWorkspaceTerminal("web"),
		app.OpenWorkspaceTerminal("loose"),
		app.OpenWorkspaceFolder("web"),
		app.SetAppAtLogin(true),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if tk, err := app.RunCatalogApp("dev1", "codex"); err != nil || !tk.OK || len(tk.Steps) != 0 {
		t.Fatalf("terminal app: %+v, %v", tk, err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	// a VM's terminal starts in ~, whatever the app's working directory
	wantTerminals := []string{home + "|ssh dev1", host + "|ssh dev1", loose + "|ssh", home + "|ssh -t dev1 codex"}
	if !slices.Equal(f.terminals, wantTerminals) {
		t.Errorf("terminals = %q, want %q", f.terminals, wantTerminals)
	}
	if !slices.Equal(f.folders, []string{host}) {
		t.Errorf("folders = %q", f.folders)
	}
	if f.atLogin == nil || !*f.atLogin {
		t.Error("the login item was not set")
	}
	if dir, err := app.PickFolder(); err != nil || dir != "picked" {
		t.Errorf("PickFolder = %q, %v", dir, err)
	}
	if len(f.runner.calls) != 0 {
		t.Errorf("window actions ran pvm: %q", f.runner.argvs())
	}
}

// TestRefusals checks that a bad name, a value the CLI would misread or
// a missing confirmation stops an action before pvm runs.
func TestRefusals(t *testing.T) {
	f := newFixture(t)
	f.saveExit(t, &exit.Entry{Name: "my-exit"})
	f.saveWorkspace(t, "web", "dev1")
	f.saveWorkspace(t, "odd", "config")
	app := f.app
	host := t.TempDir()
	create := CreateRequest{Name: "dev9", Image: "debian:13", User: "pvm", CPUs: 2, MemMB: 2048, DiskGB: 20}
	with := func(change func(*CreateRequest)) func() error {
		r := create
		change(&r)
		return func() error { _, err := app.CreateVM(r); return err }
	}
	task := func(fn func() (Task, error)) func() error {
		return func() error { _, err := fn(); return err }
	}
	for name, do := range map[string]func() error{
		"start flag":          task(func() (Task, error) { return app.StartVM("--no-wait") }),
		"start empty":         task(func() (Task, error) { return app.StartVM("") }),
		"stop path":           task(func() (Task, error) { return app.StopVM("../dev1") }),
		"delete flag":         task(func() (Task, error) { return app.DeleteVM("-y", true) }),
		"delete unconfirmed":  task(func() (Task, error) { return app.DeleteVM("dev1", false) }),
		"terminal":            func() error { return app.OpenTerminal("-T") },
		"app vm":              task(func() (Task, error) { return app.RunApp("dev 1", "gedit") }),
		"app subcommand vm":   task(func() (Task, error) { return app.RunApp("stop", "gedit") }),
		"app empty":           task(func() (Task, error) { return app.RunApp("dev1", "  ") }),
		"app control":         task(func() (Task, error) { return app.RunApp("dev1", "gedit\x00") }),
		"app quote":           task(func() (Task, error) { return app.RunApp("dev1", `sh -c "a&b"`) }),
		"catalog id":          task(func() (Task, error) { return app.RunCatalogApp("dev1", "--all") }),
		"catalog unknown":     task(func() (Task, error) { return app.RunCatalogApp("dev1", "nope") }),
		"files vm":            task(func() (Task, error) { return app.OpenFiles("ls") }),
		"config value":        task(func() (Task, error) { return app.ConfigureApps("dev1", AppConfig{Clipboard: "--drop"}) }),
		"config drop":         task(func() (Task, error) { return app.ConfigureApps("dev1", AppConfig{Drop: "true"}) }),
		"config nothing":      task(func() (Task, error) { return app.ConfigureApps("dev1", AppConfig{}) }),
		"install none":        task(func() (Task, error) { return app.InstallApps("dev1", nil) }),
		"install flag":        task(func() (Task, error) { return app.InstallApps("dev1", []string{"--yes"}) }),
		"install unknown":     task(func() (Task, error) { return app.InstallApps("dev1", []string{"nope"}) }),
		"install twice":       task(func() (Task, error) { return app.InstallApps("dev1", []string{"codex", "codex"}) }),
		"install in app name": task(func() (Task, error) { return app.InstallApps("codex", []string{"codex"}) }),
		"default vm":          task(func() (Task, error) { return app.SetDefaultVM("--clear") }),
		"route subcommand vm": task(func() (Task, error) { return app.SetVMExit("default", "my-exit", true) }),
		"route unconfirmed":   task(func() (Task, error) { return app.SetVMExit("dev1", "my-exit", false) }),
		"route unknown exit":  task(func() (Task, error) { return app.SetVMExit("dev1", "other", true) }),
		"route flag exit":     task(func() (Task, error) { return app.SetVMExit("dev1", "--via", true) }),
		"default exit":        task(func() (Task, error) { return app.SetDefaultExit("") }),
		"add exit name": task(func() (Task, error) {
			return app.AddExit(AddExitRequest{Name: "--token", Server: "vps.example.com:18443", Token: "YOUR_TOKEN"})
		}),
		"add exit direct": task(func() (Task, error) {
			return app.AddExit(AddExitRequest{Name: "direct", Server: "vps.example.com:18443", Token: "YOUR_TOKEN"})
		}),
		"add exit server": task(func() (Task, error) {
			return app.AddExit(AddExitRequest{Name: "my-exit2", Server: "vps.example.com", Token: "YOUR_TOKEN"})
		}),
		"add exit flag": task(func() (Task, error) {
			return app.AddExit(AddExitRequest{Name: "my-exit2", Server: "--token:1", Token: "YOUR_TOKEN"})
		}),
		"add exit no token": task(func() (Task, error) {
			return app.AddExit(AddExitRequest{Name: "my-exit2", Server: "vps.example.com:18443"})
		}),
		"add exit two tokens": task(func() (Task, error) {
			return app.AddExit(AddExitRequest{Name: "my-exit2", Server: "vps.example.com:18443", Token: "a b"})
		}),
		"add warp":            task(func() (Task, error) { return app.AddWarpExit("my warp") }),
		"test exit":           task(func() (Task, error) { return app.TestExit("--speed", false) }),
		"remove exit":         task(func() (Task, error) { return app.RemoveExit("my-exit", false) }),
		"remove exit name":    task(func() (Task, error) { return app.RemoveExit("-x", true) }),
		"workspace folder":    func() error { return app.OpenWorkspaceFolder("../web") },
		"workspace missing":   func() error { return app.OpenWorkspaceTerminal("none") },
		"workspace app vm":    task(func() (Task, error) { return app.RunWorkspaceApp("odd", "gedit") }),
		"workspace app empty": task(func() (Task, error) { return app.RunWorkspaceApp("web", "") }),
		"sync id":             task(func() (Task, error) { return app.SyncWorkspace("--prefer", "push") }),
		"sync direction":      task(func() (Task, error) { return app.SyncWorkspace("web", "rm") }),
		"new workspace id": task(func() (Task, error) {
			return app.CreateWorkspace(CreateWorkspaceRequest{ID: "Web", VM: "dev1", Mode: "mount", HostPath: host})
		}),
		"new workspace vm": task(func() (Task, error) {
			return app.CreateWorkspace(CreateWorkspaceRequest{ID: "api", VM: "--mode", Mode: "mount", HostPath: host})
		}),
		"new workspace mode": task(func() (Task, error) {
			return app.CreateWorkspace(CreateWorkspaceRequest{ID: "api", VM: "dev1", Mode: "--path", HostPath: host})
		}),
		"new workspace path": task(func() (Task, error) {
			return app.CreateWorkspace(CreateWorkspaceRequest{ID: "api", VM: "dev1", Mode: "mount", HostPath: "-y"})
		}),
		"new workspace ro": task(func() (Task, error) {
			return app.CreateWorkspace(CreateWorkspaceRequest{ID: "api", VM: "dev1", Mode: "sync", HostPath: host, ReadOnly: true})
		}),
		"remove workspace":    task(func() (Task, error) { return app.RemoveWorkspace("web", false) }),
		"remove workspace id": task(func() (Task, error) { return app.RemoveWorkspace("-y", true) }),
		"create name":         with(func(r *CreateRequest) { r.Name = "--no-start" }),
		"create route word":   with(func(r *CreateRequest) { r.Name, r.Exit = "explain", "my-exit" }),
		"create image":        with(func(r *CreateRequest) { r.Image = "--base" }),
		"create user":         with(func(r *CreateRequest) { r.User = "--user" }),
		"create cpus":         with(func(r *CreateRequest) { r.CPUs = 0 }),
		"create mem":          with(func(r *CreateRequest) { r.MemMB = 64 }),
		"create disk":         with(func(r *CreateRequest) { r.DiskGB = -1 }),
		"create exit":         with(func(r *CreateRequest) { r.Exit = "other" }),
		"create audio":        with(func(r *CreateRequest) { r.AppConfig.Audio = "loud" }),
		"create app":          with(func(r *CreateRequest) { r.Apps = []string{"nope"} }),
	} {
		if err := do(); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if len(f.runner.calls) != 0 {
		t.Errorf("refused actions ran pvm: %q", f.runner.argvs())
	}
	if len(f.terminals)+len(f.folders) != 0 {
		t.Errorf("refused actions opened %q %q", f.terminals, f.folders)
	}
}

func TestRemoveExitNamesItsUsers(t *testing.T) {
	f := newFixture(t)
	f.saveExit(t, &exit.Entry{Name: "my-exit"})
	f.daemon.set("/v1/clusters", `[{"id":1,"default_exit":"my-exit","vm_exits":{"dev1":"my-exit"},
		"rules":[{"type":"cidr","pattern":"203.0.113.0/24","action":"proxy","exit":"my-exit"}]}]`)
	_, err := f.app.RemoveExit("my-exit", false)
	want := "removing exit my-exit was not confirmed; it is still used by: the default exit, vm dev1, rule cidr 203.0.113.0/24"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v\nwant  %s", err, want)
	}
}

var fullCreate = CreateRequest{
	Name: "dev9", Image: "ubuntu:24.04", User: "dev", CPUs: 4, MemMB: 4096, DiskGB: 40,
	Exit: "my-exit", AppConfig: AppConfig{Clipboard: "both", Drop: "on", Audio: "out"},
	Apps: []string{"codex"},
}

// readyAfterStart makes dev9 appear running once pvm started it, and
// ready two polls later. No gateway runs before, as for the first VM.
func readyAfterStart(f *fixture) {
	gw := `{"spec":{"name":"pvm-gw1","role":"gateway"},"running":false}`
	f.daemon.set("/v1/vms", "["+gw+"]")
	polls := 0
	f.runner.during = func(c command, _ func(string)) {
		if c.argv[0] == "start" {
			f.daemon.onGet = func(path string) {
				if path != "/v1/vms" {
					return
				}
				polls++
				vm := `{"spec":{"name":"dev9","role":"user"},"running":true,"stage":{"stage":"provision"}}`
				if polls > 2 {
					vm = `{"spec":{"name":"dev9","role":"user"},"running":true,"ready":true,"stage":{"stage":"ready"}}`
				}
				f.daemon.set("/v1/vms", "["+gw+","+vm+"]")
			}
		}
	}
}

func TestCreateSetsTheExitBeforeTheFirstBoot(t *testing.T) {
	f := newFixture(t)
	f.saveExit(t, &exit.Entry{Name: "my-exit"})
	readyAfterStart(f)
	tk, err := f.app.CreateVM(fullCreate)
	if err != nil || !tk.OK {
		t.Fatalf("create: %+v, %v", tk, err)
	}
	want := []string{
		"create --name dev9 --image ubuntu:24.04 --user dev --cpus 4 --mem 4096 --disk 40 --no-start",
		"route dev9 --via my-exit",
		"app config dev9 --clipboard both --drop on --audio out",
		"start dev9 --no-wait",
		"app install dev9 codex",
	}
	if got := f.runner.argvs(); !slices.Equal(got, want) {
		t.Fatalf("ran %q\nwant %q", got, want)
	}
	var lines []string
	for _, e := range f.events {
		if out, ok := e.(map[string]any)["data"].(TaskOutput); ok {
			lines = append(lines, fmt.Sprintf("%d %s", out.Step, out.Line))
		}
	}
	if wantLines := []string{"3 dev9: starting (provision)", "3 dev9: ready"}; !slices.Equal(lines, wantLines) {
		t.Fatalf("output %q, want %q", lines, wantLines)
	}
}

// The chosen exit is set on the VM also when it is the default one, so
// the VM stays on it when the default changes later.
func TestCreatePinsTheDefaultExit(t *testing.T) {
	f := newFixture(t)
	f.saveExit(t, &exit.Entry{Name: "my-exit"})
	f.daemon.set("/v1/clusters", `[{"id":1,"default_exit":"my-exit"}]`)
	readyAfterStart(f)
	r := fullCreate
	r.AppConfig, r.Apps = AppConfig{}, nil
	tk, err := f.app.CreateVM(r)
	if err != nil || !tk.OK {
		t.Fatalf("create: %+v, %v", tk, err)
	}
	want := []string{
		"create --name dev9 --image ubuntu:24.04 --user dev --cpus 4 --mem 4096 --disk 40 --no-start",
		"route dev9 --via my-exit",
		"start dev9 --no-wait",
	}
	if got := f.runner.argvs(); !slices.Equal(got, want) {
		t.Fatalf("ran %q\nwant %q", got, want)
	}
}

func TestCreateStopsAtTheFailedStep(t *testing.T) {
	f := newFixture(t)
	f.saveExit(t, &exit.Entry{Name: "my-exit"})
	readyAfterStart(f)
	f.runner.fail = func(c command) bool { return c.argv[0] == "route" }
	tk, err := f.app.CreateVM(fullCreate)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.runner.argvs(); len(got) != 2 {
		t.Fatalf("ran %q, want create and route only", got)
	}
	want := Task{
		ID: tk.ID, Title: "Create dev9", Done: true,
		Error: "pvm: it broke (exit status 1)",
		Steps: []Step{
			{"pvm create --name dev9 --image ubuntu:24.04 --user dev --cpus 4 --mem 4096 --disk 40 --no-start", stepDone},
			{"pvm route dev9 --via my-exit", stepFailed},
			{"pvm app config dev9 --clipboard both --drop on --audio out", stepPending},
			{"pvm start dev9", stepPending},
			{"pvm app install dev9 codex", stepPending},
		},
		Remaining: []string{
			"pvm app config dev9 --clipboard both --drop on --audio out",
			"pvm start dev9",
			"pvm app install dev9 codex",
		},
	}
	if !reflect.DeepEqual(tk, want) {
		t.Fatalf("task = %+v\nwant   %+v", tk, want)
	}
	last := f.events[len(f.events)-1].(map[string]any)
	if last["event"] != eventTask || !reflect.DeepEqual(last["data"], want) {
		t.Fatalf("last event = %+v", last)
	}
	for _, argv := range f.runner.argvs() {
		if strings.HasPrefix(argv, "rm") {
			t.Fatalf("the app deleted the vm: %s", argv)
		}
	}
}

// A wait ends when the daemon stays away, and says so at once.
func TestFollowGivesUpWhenTheListCannotBeRead(t *testing.T) {
	f := newFixture(t)
	f.app.lostTimeout = 20 * time.Millisecond
	f.runner.during = func(command, func(string)) {
		f.daemon.mu.Lock()
		delete(f.daemon.routes, "/v1/vms")
		f.daemon.mu.Unlock()
	}
	start := time.Now()
	tk, err := f.app.StartVM("dev1")
	if err != nil {
		t.Fatal(err)
	}
	want := "stopped waiting for dev1, the vm list cannot be read: no route /v1/vms; once the daemon runs, `pvm start dev1` waits for it again"
	if tk.OK || tk.Error != want {
		t.Fatalf("task = %+v", tk)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("gave up after %s", time.Since(start))
	}
	var lines []string
	for _, e := range f.events {
		if out, ok := e.(map[string]any)["data"].(TaskOutput); ok {
			lines = append(lines, out.Line)
		}
	}
	if wantLines := []string{"cannot read the vm list, trying again: no route /v1/vms"}; !slices.Equal(lines, wantLines) {
		t.Fatalf("output %q, want %q", lines, wantLines)
	}
}

// A read that fails for a moment, as over a daemon restart, does not end
// the wait.
func TestFollowSurvivesADaemonRestart(t *testing.T) {
	f := newFixture(t)
	reads := 0
	f.runner.during = func(command, func(string)) {
		f.daemon.onGet = func(string) {
			reads++
			f.daemon.mu.Lock()
			defer f.daemon.mu.Unlock()
			if reads < 3 {
				delete(f.daemon.routes, "/v1/vms")
			} else {
				f.daemon.routes["/v1/vms"] = userVMs
			}
		}
	}
	if tk, err := f.app.StartVM("dev1"); err != nil || !tk.OK {
		t.Fatalf("task = %+v, %v", tk, err)
	}
}

func TestRunningTasks(t *testing.T) {
	f := newFixture(t)
	var during []string
	f.runner.during = func(command, func(string)) { during = RunningTasks(f.app) }
	if _, err := f.app.StopVM("dev1"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(during, []string{"Stop dev1"}) || len(RunningTasks(f.app)) != 0 {
		t.Fatalf("during %q, after %q", during, RunningTasks(f.app))
	}
}

func TestRemoveTokenFiles(t *testing.T) {
	f := newFixture(t)
	left := filepath.Join(f.app.p.Run, "exit-token-123")
	other := filepath.Join(f.app.p.Run, "daemon.stopped")
	for _, file := range []string{left, other} {
		if err := os.WriteFile(file, []byte("YOUR_TOKEN\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveTokenFiles(f.app.p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(left); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("token file left: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal(err)
	}
}

func TestExplorerLine(t *testing.T) {
	if got, want := explorerLine(`C:\Windows\explorer.exe`, `C:\work\a,b`), `"C:\Windows\explorer.exe" "C:\work\a,b"`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestTerminalArgv(t *testing.T) {
	got := terminalArgv(`C:\wt.exe`, `C:\src\a;calc`, []string{`C:\pvm\pvm.exe`, "ssh", "dev1", "echo a;b"})
	want := []string{`C:\wt.exe`, "-d", `C:\src\a\;calc`, `C:\pvm\pvm.exe`, "ssh", "dev1", `echo a\;b`}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := terminalArgv("wt.exe", "", []string{"pvm", "ssh"}); !slices.Equal(got, []string{"wt.exe", "pvm", "ssh"}) {
		t.Fatalf("got %q", got)
	}
}

func TestStartFollowsTheStages(t *testing.T) {
	stopped := `[{"spec":{"name":"dev1","role":"user"}}]`
	running := func(rest string) string {
		return `[{"spec":{"name":"dev1","role":"user"},"running":true` + rest + `}]`
	}
	for _, tc := range []struct {
		name  string
		polls []string
		ok    bool
		err   string
	}{
		{"ready", []string{running(""), running(`,"stage":{"stage":"provision"}`), running(`,"stage":{"stage":"agent"}`),
			running(`,"ready":true,"stage":{"stage":"ready"}`)}, true, ""},
		{"failed", []string{running(`,"stage":{"stage":"ssh"}`), running(`,"stage":{"stage":"failed","detail":"cloud-init failed; see pvm logs dev1"}`)},
			false, "dev1 failed: cloud-init failed; see pvm logs dev1"},
		{"stopped", []string{stopped}, false, "dev1 stopped before it was ready; see `pvm logs dev1`"},
		{"gone", []string{`[]`}, false, "vm dev1 does not exist"},
		{"timeout", []string{running(`,"stage":{"stage":"provision"}`)}, false,
			"dev1 was not ready after 50ms; watch it with `pvm logs dev1 --follow`"},
	} {
		f := newFixture(t)
		f.app.followTimeout = 50 * time.Millisecond
		n := 0
		f.daemon.onGet = func(string) {
			f.daemon.set("/v1/vms", tc.polls[min(n, len(tc.polls)-1)])
			n++
		}
		tk, err := f.app.StartVM("dev1")
		if err != nil {
			t.Fatal(err)
		}
		if tk.OK != tc.ok || tk.Error != tc.err {
			t.Errorf("%s: ok %v error %q, want %v %q", tc.name, tk.OK, tk.Error, tc.ok, tc.err)
		}
		if tk.Steps[0].Command != "pvm start dev1" {
			t.Errorf("%s: step %q", tc.name, tk.Steps[0].Command)
		}
	}
}

// TestSecretsStayInTheBackend runs an action that uses a token against
// stores that hold keys, and looks for them in everything the page gets.
func TestSecretsStayInTheBackend(t *testing.T) {
	const token = "YOUR_TOKEN"
	secrets := []string{token, "PRIVATE_KEY", "WARP_TOKEN", "WARP_LICENSE", "WARP_DEVICE", "FINGERPRINT", "203.0.113.7"}
	f := newFixture(t)
	f.saveExit(t, &exit.Entry{Name: "my-exit", Server: "vps.example.com:18443", Token: token, Fingerprint: "FINGERPRINT",
		WGPrivateKey: "PRIVATE_KEY", Endpoint: "vps.example.com:51820", TunnelIP: "203.0.113.7"})
	f.saveExit(t, &exit.Entry{Name: "my-warp", Provider: exit.ProviderWARP,
		WARP: &exit.WARPAccount{DeviceID: "WARP_DEVICE", Token: "WARP_TOKEN", License: "WARP_LICENSE"}, WGPrivateKey: "PRIVATE_KEY"})

	tokenFile := ""
	f.runner.during = func(c command, line func(string)) {
		if slices.Contains(c.argv, token) {
			t.Errorf("the token is on the command line: %q", c.argv)
		}
		tokenFile = c.argv[len(c.argv)-1]
		if filepath.Dir(tokenFile) != f.app.p.Run {
			t.Errorf("token file %s is not in the run directory", tokenFile)
		}
		fi, err := os.Stat(tokenFile)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("token file mode %v", fi.Mode())
		}
		if data, _ := os.ReadFile(tokenFile); strings.TrimSpace(string(data)) != token {
			t.Errorf("token file holds %q", data)
		}
		line("enrolling with token " + token)
	}
	var page []any
	for _, failing := range []bool{false, true} {
		f.runner.fail = func(command) bool { return failing }
		tk, err := f.app.AddExit(AddExitRequest{Name: "my-exit2", Server: "vps.example.com:18443", Token: token})
		if err != nil || tk.OK == failing {
			t.Fatalf("add exit: %+v, %v", tk, err)
		}
		ran := f.runner.argvs()
		if want := "exit add my-exit2 --server vps.example.com:18443 --token-file " + tokenFile; ran[len(ran)-1] != want {
			t.Fatalf("ran %q, want %q", ran[len(ran)-1], want)
		}
		if _, err := os.Stat(tokenFile); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("token file left behind (failing=%v): %v", failing, err)
		}
		page = append(page, tk)
	}
	if left, _ := filepath.Glob(filepath.Join(f.app.p.Run, "exit-token-*")); len(left) != 0 {
		t.Fatalf("token files left: %q", left)
	}

	exits, err := f.app.Exits()
	if err != nil {
		t.Fatal(err)
	}
	_, removeErr := f.app.RemoveExit("my-exit", false)
	state, err := f.app.State()
	if err != nil {
		t.Fatal(err)
	}
	page = append(page, exits, removeErr.Error(), state, f.events)
	data, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "enrolling with token ***") {
		t.Fatalf("the output line is missing: %s", data)
	}
	for _, s := range secrets {
		if strings.Contains(string(data), s) {
			t.Errorf("%q reached the page: %s", s, data)
		}
	}
}

// boundMethods is everything the page can call. A new exported method
// on App is reachable from JavaScript; add it here on purpose.
var boundMethods = []string{
	"AddExit", "AddWarpExit", "Catalog", "ClearDefaultVM", "ConfigureApps", "CreateDefaults", "CreateVM",
	"CreateWorkspace", "DefaultExit", "DeleteVM", "Exits", "Images", "InstallApps", "InstalledApps", "OpenFiles",
	"OpenTerminal", "OpenWorkspaceFiles", "OpenWorkspaceFolder", "OpenWorkspaceTerminal", "PickFolder",
	"RemoveExit", "RemoveWorkspace", "RunApp", "RunCatalogApp", "RunWorkspaceApp", "SetAppAtLogin",
	"SetDaemonAtLogin", "SetDefaultExit", "SetDefaultVM", "SetVMExit", "Settings", "StartDaemon", "StartVM",
	"State", "StopVM", "SyncWorkspace", "TestExit", "VMs", "Workspaces",
}

func TestBoundMethods(t *testing.T) {
	typ := reflect.TypeFor[*App]()
	var got []string
	for i := range typ.NumMethod() {
		got = append(got, typ.Method(i).Name)
	}
	if !slices.Equal(got, boundMethods) {
		t.Fatalf("bound methods changed:\n got %q\nwant %q", got, boundMethods)
	}
	for i := range typ.Elem().NumField() {
		if f := typ.Elem().Field(i); f.IsExported() {
			t.Errorf("field %s is exported", f.Name)
		}
	}
}

// TestWSLRefusal calls every bound method with zero arguments under the
// WSL2 backend: only State answers.
func TestWSLRefusal(t *testing.T) {
	f := newFixture(t)
	f.app.backend = func() (string, error) { return "wsl", nil }
	st, err := f.app.State()
	if err != nil || st.Backend != "wsl" || st.DaemonHealthy {
		t.Fatalf("state = %+v, %v", st, err)
	}
	v := reflect.ValueOf(f.app)
	for i := range v.NumMethod() {
		name := v.Type().Method(i).Name
		if name == "State" {
			continue
		}
		m := v.Method(i)
		args := make([]reflect.Value, m.Type().NumIn())
		for j := range args {
			args[j] = reflect.Zero(m.Type().In(j))
		}
		out := m.Call(args)
		if err, _ := out[len(out)-1].Interface().(error); !errors.Is(err, errWSL) {
			t.Errorf("%s: err = %v, want the WSL refusal", name, err)
		}
	}
	if len(f.runner.calls)+len(f.terminals)+len(f.folders) != 0 || f.atLogin != nil {
		t.Error("an action ran under the WSL backend")
	}
}

// TestPvmRunner runs a stand-in for pvm: arguments reach it unchanged,
// both streams come back as lines and the exit code is reported.
func TestPvmRunner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in is a shell script")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "pvm")
	// pwd -P resolves links as EvalSymlinks does below: the macOS temporary
	// folder is under /var, a link to /private/var.
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\"\npwd -P\necho oops >&2\nexit 3\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	r := pvmRunner{find: func() (string, error) { return script, nil }}
	var mu sync.Mutex
	var lines []string
	code, err := r.run(command{argv: []string{"app", "dev1", "--", "a b", "$(id)", "--x"}, dir: dir}, func(s string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, s)
	})
	if err != nil || code != 3 {
		t.Fatalf("code %d, err %v", code, err)
	}
	slices.Sort(lines)
	want := []string{"$(id)", "--", "--x", "a b", "app", "dev1", "oops"}
	if cwd, err := filepath.EvalSymlinks(dir); err == nil {
		want = append(want, cwd)
	}
	slices.Sort(want)
	if !slices.Equal(lines, want) {
		t.Fatalf("lines = %q, want %q", lines, want)
	}

	r.find = func() (string, error) { return "", errors.New("pvm.exe is not next to PVM Desktop") }
	if _, err := r.run(command{argv: []string{"ls"}}, func(string) {}); err == nil {
		t.Fatal("a missing pvm ran")
	}
}

func TestLineWriter(t *testing.T) {
	var got []string
	w := &lineWriter{line: func(s string) { got = append(got, s) }}
	for _, chunk := range []string{"one\r\ntw", "o\rthree\n\nlast"} {
		w.Write([]byte(chunk))
	}
	w.flush()
	if want := []string{"one", "two", "three", "last"}; !slices.Equal(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
}

func TestCommandLine(t *testing.T) {
	if got, want := commandLine("app", "dev1", "--", "say", `a "b" c`, ""), `pvm app dev1 -- say "a \"b\" c" ""`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	if got, want := commandLine("app", "dev1", "https://example.com/?a=1&b=2"), `pvm app dev1 "https://example.com/?a=1&b=2"`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	if got, want := commandLine("workspace", "create", "proj", "dev1", "--path", `C:\R&D\proj`), `pvm workspace create proj dev1 --path "C:\R&D\proj"`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	// a backslash before the closing quote would make it a literal one
	if got, want := commandLine("workspace", "create", "w", "dev1", "--path", `C:\my dir\`, "-y"), `pvm workspace create w dev1 --path "C:\my dir\\" -y`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	if got, want := commandLine("x", `a\\"b c\`, `C:\dir\`), `pvm x "a\\\\\"b c\\" C:\dir\`; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}
