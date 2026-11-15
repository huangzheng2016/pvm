package daemon

import (
	"os/exec"
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
	} {
		if err := validateAppRequest(&bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	for _, ok := range []appStartRequest{
		{Argv: []string{"gedit", "--new-window"}},
		{URL: "https://mail.example.com/inbox", Kiosk: true, ID: "mail"},
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
		"install": appInstallScript(),
		"server":  appServerScript(),
		"cmd":     appLaunchScript(&appRecord{ID: "gedit", Kind: "cmd", Argv: []string{"gedit", "it's a file; rm -rf ~", "$(id)"}}),
		"url":     appLaunchScript(&appRecord{ID: "mail", Kind: "url", URL: "https://example.com/?a=1&b='2'", Kiosk: true}),
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
	if !strings.Contains(stop, appUnitPrefix+"gedit") || !strings.Contains(stop, `wmctrl -i -c "$w"`) || !strings.Contains(stop, "'0x00c00003'") || !strings.Contains(stop, "stop "+webappUnit) {
		t.Errorf("stop script:\n%s", stop)
	}
	if strings.Contains(appStopScript([]appRecord{{ID: "mail", Kind: "url", Window: "0x1"}}, true), "stop "+webappUnit) {
		t.Error("the shared Firefox must keep running while other web apps use it")
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
	if _, code, _ := s.clipTarget(spec, "clip:../../x"); code != 400 {
		t.Errorf("bad clipboard kind accepted (code %d)", code)
	}
	if got, _, err := s.clipTarget(spec, "echo"); err != nil || got != "echo" {
		t.Errorf("other targets must pass through: %q, %v", got, err)
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
