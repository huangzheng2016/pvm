package apps

import (
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

func TestCatalog(t *testing.T) {
	var ids []string
	for _, a := range List() {
		ids = append(ids, a.ID)
		got, ok := Get(a.ID)
		if !ok || got != a {
			t.Fatalf("Get(%q) = %+v, %v", a.ID, got, ok)
		}
	}
	if want := []string{"claude-code", "claude-desktop", "codex"}; !slices.Equal(ids, want) {
		t.Fatalf("catalog ids = %v, want %v", ids, want)
	}
	if _, ok := Get("nope"); ok {
		t.Fatal("Get of an unknown id succeeded")
	}
	if a, _ := Get("claude-desktop"); !a.Window || a.Start != "claude-desktop" || a.Source != "official" {
		t.Fatalf("claude-desktop = %+v", a)
	}
}

// An install script must leave an installed app alone, so the download and
// install steps sit behind a check, and it ends by showing the version.
func TestScripts(t *testing.T) {
	guard := regexp.MustCompile(`(?m)^if ! dpkg -s \S+ >/dev/null 2>&1; then$|^if \[ ! -x "[^"]+" \]; then$`)
	for _, a := range List() {
		t.Run(a.ID, func(t *testing.T) {
			if !strings.HasPrefix(a.Script, "set -euo pipefail\n") {
				t.Fatal("script does not start with the prologue")
			}
			if strings.Contains(a.Script, "\r") {
				t.Fatal("script has carriage returns")
			}
			if !guard.MatchString(a.Script) {
				t.Fatal("script has no installed check")
			}
			if strings.Contains(a.Script, "sudo") {
				t.Fatal("script runs as root and must not call sudo")
			}
			if runtime.GOOS == "windows" {
				t.Skip("needs bash")
			}
			cmd := exec.Command("bash", "-n")
			cmd.Stdin = strings.NewReader(a.Script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("bash -n: %v\n%s", err, out)
			}
		})
	}
}

func TestLoadValidation(t *testing.T) {
	const good = "name: Dev\ndescription: d\nhomepage: https://example.com\nsource: official\nwindow: true\nstart: dev\n"
	fsys := func(id, yaml, script string) fstest.MapFS {
		return fstest.MapFS{
			"prologue.sh":       {Data: []byte("set -e\n")},
			id + "/app.yaml":    {Data: []byte(yaml)},
			id + "/install.sh":  {Data: []byte(script)},
			"unrelated-file.md": {Data: []byte("x")},
		}
	}
	list, err := load(fsys("dev", good, "true\n"))
	if err != nil || len(list) != 1 || list[0].ID != "dev" || list[0].Script != "set -e\ntrue\n" || !list[0].Window {
		t.Fatalf("load = %+v, %v", list, err)
	}
	for _, tc := range []struct{ name, id, yaml, script, want string }{
		{"unknown key", "dev", good + "extra: 1\n", "true\n", "field extra not found"},
		{"bad id", "Dev_1", good, "true\n", "invalid app id"},
		{"empty name", "dev", strings.Replace(good, "name: Dev", "name: ''", 1), "true\n", "name is empty"},
		{"empty start", "dev", strings.Replace(good, "start: dev", "start: ' '", 1), "true\n", "start is empty"},
		{"bad source", "dev", strings.Replace(good, "official", "vendor", 1), "true\n", "not official or community"},
		{"bad homepage", "dev", strings.Replace(good, "https://", "http://", 1), "true\n", "not an https URL"},
		{"empty script", "dev", good, " \n", "install.sh is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := load(fsys(tc.id, tc.yaml, tc.script)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("load error = %v, want %q", err, tc.want)
			}
		})
	}
	missing := fsys("dev", good, "true\n")
	delete(missing, "dev/install.sh")
	if _, err := load(missing); err == nil {
		t.Fatal("load without install.sh succeeded")
	}
}
