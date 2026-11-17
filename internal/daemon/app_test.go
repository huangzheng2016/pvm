package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/huangzheng2016/pvm/internal/vm"
)

func TestValidateAppRequest(t *testing.T) {
	for _, bad := range []appStartRequest{
		{},
		{URL: "https://example.com", Argv: []string{"gedit"}},
		{URL: "file:///etc/passwd"},
		{URL: "javascript:alert(1)"},
		{Argv: []string{""}},
		{Argv: []string{"gedit", "a\x00b"}},
		{Argv: []string{"gedit"}, ID: "Bad_Name"},
		{Argv: []string{"gedit"}, Kiosk: true},
		{Argv: []string{"gedit"}, Cwd: "/workspace/web"},
		{Argv: []string{"gedit"}, CwdRoot: "/workspace/web"},
		{Argv: []string{"gedit"}, Cwd: "/home/u", CwdRoot: "/home/u"},
		{Argv: []string{"gedit"}, Cwd: "/workspace", CwdRoot: "/workspace"},
		{Argv: []string{"gedit"}, Cwd: "/workspace/Web", CwdRoot: "/workspace/Web"},
		{Argv: []string{"gedit"}, Cwd: "/workspace/web/a", CwdRoot: "/workspace/web/a"},
		{Argv: []string{"gedit"}, Cwd: "/workspace/web/../api", CwdRoot: "/workspace/web"},
		{Argv: []string{"gedit"}, Cwd: "/workspace/web/..", CwdRoot: "/workspace/web"},
		{Argv: []string{"gedit"}, Cwd: "/workspace/web/./a", CwdRoot: "/workspace/web"},
		{Argv: []string{"gedit"}, Cwd: "/workspace/web//a", CwdRoot: "/workspace/web"},
		{Argv: []string{"gedit"}, Cwd: "/workspace/web/", CwdRoot: "/workspace/web"},
		{Argv: []string{"gedit"}, Cwd: "/workspace/web2", CwdRoot: "/workspace/web"},
		{Argv: []string{"gedit"}, Cwd: "/workspace/api", CwdRoot: "/workspace/web"},
		{Argv: []string{"gedit"}, Cwd: "workspace/web", CwdRoot: "workspace/web"},
		{Argv: []string{"gedit"}, Cwd: "/workspace/web/a\nb", CwdRoot: "/workspace/web"},
		{Argv: []string{"gedit"}, Cwd: "/workspace/web/a\x00b", CwdRoot: "/workspace/web"},
		{Argv: []string{"gedit"}, Cwd: "/workspace/web/a\x1bb", CwdRoot: "/workspace/web"},
		{URL: "https://example.com", Cwd: "/workspace/web", CwdRoot: "/workspace/web"},
	} {
		if err := validateAppRequest(&bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	for _, ok := range []appStartRequest{
		{Argv: []string{"gedit", "--new-window"}},
		{URL: "https://mail.example.com/inbox", Kiosk: true, ID: "mail"},
		{Argv: []string{"gedit"}, Cwd: "/workspace/web", CwdRoot: "/workspace/web"},
		{Argv: []string{"gedit"}, Cwd: "/workspace/web/it's a dir/文档/..a", CwdRoot: "/workspace/web"},
	} {
		if err := validateAppRequest(&ok); err != nil {
			t.Errorf("refused %+v: %v", ok, err)
		}
	}
}

func TestAppID(t *testing.T) {
	for in, want := range map[*appStartRequest]string{
		{Argv: []string{"/usr/bin/gnome-text-editor"}}:      "gnome-text-editor",
		{URL: "https://www.example.com/a?b"}:                "example-com",
		{Argv: []string{"LibreOffice"}}:                     "libreoffice",
		{Argv: []string{"..."}}:                             "app",
		{URL: "https://a-very-long-subdomain.example.com/"}: "a-very-long-subdomain-ex",
	} {
		if got := appID(in); got != want || !appIDRe.MatchString(got) {
			t.Errorf("appID(%+v) = %q, want %q", in, got, want)
		}
	}
}

// Arguments reach the guest shell quoted, whatever they contain.
func TestAppScriptsParse(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	scripts := map[string]string{
		"install": appInstallScript(true),
		"server":  appServerScript(),
		"cmd":     appLaunchScript(&appRecord{ID: "gedit", Kind: "cmd", Argv: []string{"gedit", "it's a file; rm -rf ~", "$(id)"}}, "", ""),
		"cwd":     appLaunchScript(&appRecord{ID: "gedit", Kind: "cmd", Argv: []string{"gedit"}}, "/workspace/web/it's a dir; $(id)", "/workspace/web"),
		"url":     appLaunchScript(&appRecord{ID: "mail", Kind: "url", URL: "https://example.com/?a=1&b='2'", Kiosk: true}, "", ""),
		"stop":    appStopScript([]appRecord{{ID: "gedit"}, {ID: "mail", Kind: "url", Window: "0x00c00003"}}, false),
	}
	for name, s := range scripts {
		if out, err := exec.Command(sh, "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("%s script: %v: %s", name, err, out)
		}
	}
	if !strings.Contains(scripts["cmd"], `'it'"'"'s a file; rm -rf ~'`) || !strings.Contains(scripts["cmd"], `'$(id)'`) {
		t.Errorf("cmd arguments not quoted:\n%s", scripts["cmd"])
	}
	if strings.Contains(scripts["cmd"], "--working-directory") {
		t.Errorf("cmd without a cwd sets a working directory:\n%s", scripts["cmd"])
	}
	if !strings.Contains(scripts["cwd"], `dir='/workspace/web/it'"'"'s a dir; $(id)'`) || !strings.Contains(scripts["cwd"], ` --working-directory="$dir" `) {
		t.Errorf("cwd not quoted or not passed:\n%s", scripts["cwd"])
	}
	// GTK apps leave their title bar to the host when gtk3-nocsd is there
	if !strings.Contains(scripts["cmd"], `$nocsd -- "$prog" $nogpu "$@"`) || !strings.Contains(scripts["cmd"], "v8_context_snapshot.bin") || !strings.Contains(scripts["install"], "install -y --no-install-recommends gtk3-nocsd") {
		t.Errorf("gtk3-nocsd not installed or preloaded: %s | %s", scripts["install"], scripts["cmd"])
	}
	// every web app shares one profile; a running Firefox gets a new window
	url := scripts["url"]
	for _, want := range []string{`/pvm/webapps/profile"`, `--new-window 'https://example.com/?a=1&b='"'"'2'"'"''`, "--class " + webappClass, "add,fullscreen", `echo "WINDOW=$win"`} {
		if !strings.Contains(url, want) {
			t.Errorf("url launch lacks %q:\n%s", want, url)
		}
	}
	if strings.Contains(url, "--no-remote") || strings.Contains(url, "--new-instance") {
		t.Errorf("url launch must reuse the shared Firefox:\n%s", url)
	}
	stop := scripts["stop"]
	if !strings.Contains(stop, appUnitPrefix+"gedit") || !strings.Contains(stop, `wmctrl -i -c "$w"`) || !strings.Contains(stop, "'0x00c00003'") || !strings.Contains(stop, "stop_units "+webappUnit) {
		t.Errorf("stop script:\n%s", stop)
	}
	if !strings.Contains(stop, "is-active --quiet") {
		t.Errorf("stop script must tolerate units that never started:\n%s", stop)
	}
	if strings.Contains(appStopScript([]appRecord{{ID: "mail", Kind: "url", Window: "0x1"}}, true), "stop "+webappUnit) {
		t.Error("the shared Firefox must keep running while other web apps use it")
	}
}

// fakeAppBin is a PATH directory where systemd-run records its options
// in opts and the command after "--" in argv, one word per line.
func fakeAppBin(t *testing.T) (bin, opts, argv string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	bin = t.TempDir()
	opts, argv = filepath.Join(bin, "opts"), filepath.Join(bin, "argv")
	writeScript(t, filepath.Join(bin, "systemd-run"), `: > "`+opts+`"; while [ "$1" != "--" ]; do printf '%s\n' "$1" >> "`+opts+`"; shift; done; shift; printf '%s\n' "$@" > "`+argv+`"`)
	writeScript(t, filepath.Join(bin, "systemctl"), "exit 0")
	writeScript(t, filepath.Join(bin, "sleep"), "exit 0")
	writeScript(t, filepath.Join(bin, "plain"), "exit 0")
	return bin, opts, argv
}

func writeScript(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// The launch script keeps every argument and adds --disable-gpu only to
// programs that ship Chromium's V8 snapshot beside them.
func TestAppLaunchDisablesGPUForChromium(t *testing.T) {
	bin, _, argv := fakeAppBin(t)
	write := func(path, body string) { writeScript(t, path, body) }
	app := t.TempDir()
	write(filepath.Join(app, "electron"), "exit 0")
	if err := os.WriteFile(filepath.Join(app, "v8_context_snapshot.bin"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(app, "electron"), filepath.Join(bin, "electron")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		prog string
		want []string
	}{
		{"plain", []string{"plain", "a b", "$(id)"}},
		{"electron", []string{"electron", "--disable-gpu", "a b", "$(id)"}},
	} {
		script := appLaunchScript(&appRecord{ID: "x", Kind: "cmd", Argv: []string{tc.prog, "a b", "$(id)"}}, "", "")
		c := exec.Command("sh", "-c", script)
		c.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", tc.prog, err, out)
		}
		raw, err := os.ReadFile(argv)
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
		got[0] = filepath.Base(got[0])
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("%s: argv = %q, want %q", tc.prog, got, tc.want)
		}
	}
}

// A command starts in its workspace folder, in the workspace root when
// the folder is missing in the VM, and not at all without the root.
func TestAppLaunchWorkingDirectory(t *testing.T) {
	bin, opts, _ := fakeAppBin(t)
	root := filepath.Join(t.TempDir(), "web")
	dir := filepath.Join(root, `it's a "dir"; $(id)`)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(root, "not synced")
	for _, tc := range []struct {
		name, cwd, root string
		wantDir         string // "" = no --working-directory
		wantOut         string
		wantExit        int
	}{
		{"no cwd", "", "", "", "", 0},
		{"root", root, root, root, "", 0},
		{"folder", dir, root, dir, "", 0},
		{"missing folder", gone, root, root, appCwdFallback, 0},
		{"missing root", gone, gone, "", appCwdMissing, 4},
	} {
		os.Remove(opts)
		c := exec.Command("sh", "-c", appLaunchScript(&appRecord{ID: "x", Kind: "cmd", Argv: []string{"plain"}}, tc.cwd, tc.root))
		c.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin")
		out, err := c.Output()
		if c.ProcessState.ExitCode() != tc.wantExit {
			t.Fatalf("%s: exit %d (%v), want %d: %s", tc.name, c.ProcessState.ExitCode(), err, tc.wantExit, out)
		}
		if got := strings.TrimSpace(string(out)); got != tc.wantOut || hasLine(string(out), appCwdFallback) != (tc.wantOut == appCwdFallback) {
			t.Errorf("%s: stdout = %q, want %q", tc.name, got, tc.wantOut)
		}
		raw, err := os.ReadFile(opts)
		if tc.wantExit != 0 {
			if err == nil {
				t.Errorf("%s: the app was started: %s", tc.name, raw)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, o := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
			if d, ok := strings.CutPrefix(o, "--working-directory="); ok {
				got = append(got, d)
			}
		}
		if (tc.wantDir == "" && got != nil) || (tc.wantDir != "" && (len(got) != 1 || got[0] != tc.wantDir)) {
			t.Errorf("%s: working directory = %q, want %q; options:\n%s", tc.name, got, tc.wantDir, raw)
		}
	}
}

func TestWebappWindow(t *testing.T) {
	if w, ok := webappWindow("noise\nWINDOW=0x00c0001d\n"); !ok || w != "0x00c0001d" {
		t.Errorf("window = %q, %v", w, ok)
	}
	for _, bad := range []string{"", "WINDOW=", "WINDOW=12345", "WINDOW=$(id)"} {
		if _, ok := webappWindow(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}

// The host clipboard enters a VM only when its policy allows it, and the
// guest target is bound to the VM user.
func TestClipTargetFollowsPolicy(t *testing.T) {
	spec := &vm.Spec{Name: "dev1", User: "u", GuestUID: 1000}
	s := newAuditTestServer(t, spec)
	for _, mode := range []string{"", "to-host"} {
		if err := s.saveAppState("dev1", &appState{Clipboard: mode}); err != nil {
			t.Fatal(err)
		}
		if _, code, err := s.clipTarget(spec, "clip:text"); err == nil || code != 403 {
			t.Errorf("policy %q: clip target allowed (code %d)", mode, code)
		}
	}
	for _, mode := range []string{"to-guest", "both"} {
		if err := s.saveAppState("dev1", &appState{Clipboard: mode}); err != nil {
			t.Fatal(err)
		}
		got, _, err := s.clipTarget(spec, "clip:png")
		if err != nil || got != "clip:1000:png" {
			t.Errorf("policy %q: target %q, %v", mode, got, err)
		}
	}
	// files from the guest clipboard need the other direction
	for mode, allowed := range map[string]bool{"": false, "to-guest": false, "to-host": true, "both": true} {
		if err := s.saveAppState("dev1", &appState{Clipboard: mode}); err != nil {
			t.Fatal(err)
		}
		got, code, err := s.clipTarget(spec, "clipout:files")
		if allowed != (err == nil) || (allowed && got != "clipout:1000:files") || (!allowed && code != 403) {
			t.Errorf("policy %q: clipout target %q, code %d, %v", mode, got, code, err)
		}
	}
	if _, code, _ := s.clipTarget(spec, "clip:../../x"); code != 400 {
		t.Errorf("bad clipboard kind accepted (code %d)", code)
	}
	if got, _, err := s.clipTarget(spec, "echo"); err != nil || got != "echo" {
		t.Errorf("other targets must pass through: %q, %v", got, err)
	}
}

func TestAppSecretPersistsAndRejectsCorruption(t *testing.T) {
	s := newAuditTestServer(t, &vm.Spec{Name: "dev1", User: "u"})
	_, secret, err := s.appSecret("dev1")
	if err != nil {
		t.Fatal(err)
	}
	_, again, err := s.appSecret("dev1")
	if err != nil || again != secret {
		t.Fatalf("valid secret changed: %v", err)
	}
	path := filepath.Join(s.appDir("dev1"), "xpra.secret")
	if err := os.WriteFile(path, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.appSecret("dev1"); err == nil {
		t.Fatal("corrupt secret was silently replaced")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "broken" {
		t.Fatalf("corrupt secret was overwritten: %q", data)
	}
}

func TestStreamTargetAcceptsMacOSGuestUID(t *testing.T) {
	if !validStreamTarget("xpra:501") {
		t.Fatal("macOS guest uid 501 rejected")
	}
	if validStreamTarget("xpra:0") {
		t.Fatal("root uid accepted")
	}
}

func TestLineWriterSplitsLinesAndProgress(t *testing.T) {
	var got []string
	lw := lineWriter(func(l string) { got = append(got, l) })
	lw.Write([]byte("Get:1 a\nGet:2 b\r\nProgress 10%\rProgress 20%\r"))
	lw.Write([]byte("Setting up x"))
	lw.Write([]byte("pra\n\n  \ntail"))
	lw.Close()
	want := []string{"Get:1 a", "Get:2 b", "Progress 10%", "Progress 20%", "Setting up xpra", "tail"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("lines = %q, want %q", got, want)
	}
}
