package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A command started inside a workspace is sent with its guest folder and
// the workspace root; a web app and a start outside a workspace are not.
func TestAppStartSendsWorkspaceFolder(t *testing.T) {
	var body map[string]any
	note := ""
	p, _ := fakeVMDaemonWith(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/v1/vms/dev1/apps/prepare":
			fmt.Fprintln(w, `{"done":true}`)
		case "/v1/vms/dev1/apps":
			body = nil
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(map[string]any{"app": map[string]any{"id": "gedit"}, "note": note})
		default:
			return false
		}
		return true
	}, "dev1")
	host := t.TempDir()
	sub := filepath.Join(host, "my docs", "it's")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	saveWorkspace(t, p, "web", "dev1", host)
	start := func(args ...string) string {
		t.Helper()
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		old := os.Stderr
		os.Stderr = w
		code, _ := captureStdout(t, func() int { return cmdAppStart(p, append([]string{"--no-attach"}, args...)) })
		os.Stderr = old
		w.Close()
		errOut, _ := io.ReadAll(r)
		r.Close()
		if code != 0 {
			t.Fatalf("pvm app %q: exit %d: %s", args, code, errOut)
		}
		return string(errOut)
	}

	t.Chdir(sub)
	note = "/workspace/web/my docs/it's is not in the vm; starting in /workspace/web"
	errOut := start("--", "gedit")
	if body["cwd"] != "/workspace/web/my docs/it's" || body["cwd_root"] != "/workspace/web" {
		t.Errorf("in a workspace folder: body = %v", body)
	}
	if !strings.Contains(errOut, "pvm: "+note+"\n") {
		t.Errorf("note not printed: %q", errOut)
	}
	note = ""
	t.Chdir(host)
	if start("dev1", "--", "gedit"); body["cwd"] != "/workspace/web" || body["cwd_root"] != "/workspace/web" {
		t.Errorf("in the workspace root: body = %v", body)
	}
	start("https://example.com")
	if _, ok := body["cwd"]; ok || body["cwd_root"] != nil {
		t.Errorf("web app: body = %v", body)
	}
	t.Chdir(t.TempDir())
	start("dev1", "--", "gedit")
	if _, ok := body["cwd"]; ok || body["cwd_root"] != nil {
		t.Errorf("outside a workspace: body = %v", body)
	}
}

func TestFlagsFirstLeavesBoolFlagsAlone(t *testing.T) {
	fs := flag.NewFlagSet("app", flag.ContinueOnError)
	fs.String("name", "", "")
	fs.Bool("kiosk", false, "")
	got := flagsFirst(fs, []string{"dev1", "--kiosk", "https://example.com", "--name", "mail"})
	want := []string{"--kiosk", "--name", "mail", "--", "dev1", "https://example.com"}
	if !slices.Equal(got, want) {
		t.Fatalf("flagsFirst = %q, want %q", got, want)
	}
	if err := fs.Parse(got); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fs.Args(), []string{"dev1", "https://example.com"}) {
		t.Fatalf("positionals = %q", fs.Args())
	}
}

func TestSplitInstallArgs(t *testing.T) {
	catalog := []catalogApp{{ID: "claude-code"}, {ID: "codex"}, {ID: "claude-desktop", Window: true}}
	for _, tc := range []struct {
		args    []string
		vm      string
		ids     []string
		wantErr string
	}{
		{args: []string{"codex"}, ids: []string{"codex"}},
		{args: []string{"codex", "claude-code"}, ids: []string{"codex", "claude-code"}},
		{args: []string{"dev1", "codex"}, vm: "dev1", ids: []string{"codex"}},
		{args: []string{"dev1", "claude-desktop", "codex"}, vm: "dev1", ids: []string{"claude-desktop", "codex"}},
		{args: nil, wantErr: "usage: pvm app install"},
		{args: []string{"dev1"}, wantErr: `unknown app "dev1"`},
		{args: []string{"dev1", "gedit"}, wantErr: `unknown app "gedit"`},
		{args: []string{"codex", "dev1"}, wantErr: `unknown app "dev1"`},
	} {
		vm, list, err := splitInstallArgs(tc.args, catalog)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%q: error = %v, want %q", tc.args, err, tc.wantErr)
			}
			continue
		}
		var ids []string
		for _, a := range list {
			ids = append(ids, a.ID)
		}
		if err != nil || vm != tc.vm || !slices.Equal(ids, tc.ids) {
			t.Errorf("%q: got %q %v, %v; want %q %v", tc.args, vm, ids, err, tc.vm, tc.ids)
		}
	}
}

// captureStderr runs fn with os.Stderr redirected and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = old
	w.Close()
	out, _ := io.ReadAll(r)
	r.Close()
	return string(out)
}

// `pvm app install` sends one request per app, prepares a window app
// first, prints the script output and stops at the first failure.
func TestAppInstallAndList(t *testing.T) {
	var reqs []string
	fakeVMDaemonWith(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch r.URL.Path {
		case "/v1/apps/catalog":
			fmt.Fprint(w, `[{"id":"claude-desktop","name":"Claude Desktop","description":"desktop app","source":"official","window":true},
				{"id":"codex","name":"Codex CLI","description":"coding agent","source":"official","window":false},
				{"id":"claude-code","name":"Claude Code","description":"coding agent","source":"official","window":false}]`)
		case "/v1/vms/dev1/apps/prepare":
			reqs = append(reqs, "prepare")
			fmt.Fprintln(w, `{"done":true}`)
		case "/v1/vms/dev1/apps/install":
			var body struct {
				IDs []string `json:"ids"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			reqs = append(reqs, "install "+strings.Join(body.IDs, ","))
			id := body.IDs[0]
			fmt.Fprintf(w, `{"app":%q,"state":"installing"}`+"\n"+`{"app":%q,"out":"line from %s"}`+"\n", id, id, id)
			if id == "codex" {
				fmt.Fprintln(w, `{"done":true,"error":"install codex in dev1: the install script failed with exit status 1"}`)
			} else {
				fmt.Fprintf(w, `{"app":%q,"state":"installed"}`+"\n"+`{"done":true,"installed":[%q]}`+"\n", id, id)
			}
		case "/v1/apps":
			fmt.Fprint(w, `{"apps":[{"vm":"dev1","id":"gedit","kind":"cmd","argv":["gedit"],"state":"running"}],"installed":{"dev2":["codex"],"dev1":["claude-code","claude-desktop"]}}`)
		default:
			return false
		}
		return true
	}, "dev1")
	t.Chdir(t.TempDir())

	var code int
	var out string
	errOut := captureStderr(t, func() {
		code, out = captureStdout(t, func() int { return CmdApp([]string{"install", "claude-desktop", "codex", "claude-code"}) })
	})
	if code != 1 || out != "installed claude-desktop in dev1\n" {
		t.Errorf("install: exit %d, stdout %q", code, out)
	}
	if want := []string{"prepare", "install claude-desktop", "install codex"}; !slices.Equal(reqs, want) {
		t.Errorf("requests = %v, want %v", reqs, want)
	}
	for _, want := range []string{
		"installing claude-desktop in dev1...\n        | line from claude-desktop\n",
		"        | line from codex\n",
		"pvm: install codex in dev1: the install script failed with exit status 1; not attempted: claude-code\n",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%s", want, errOut)
		}
	}

	reqs = nil
	errOut = captureStderr(t, func() {
		code, out = captureStdout(t, func() int { return CmdApp([]string{"install", "dev1", "claude-code"}) })
	})
	if code != 0 || out != "installed claude-code in dev1\n" || !slices.Equal(reqs, []string{"install claude-code"}) {
		t.Errorf("install with a vm: exit %d, stdout %q, requests %v\n%s", code, out, reqs, errOut)
	}

	reqs = nil
	errOut = captureStderr(t, func() {
		code, _ = captureStdout(t, func() int { return CmdApp([]string{"install", "gedit"}) })
	})
	if code != 1 || len(reqs) != 0 || !strings.Contains(errOut, `unknown app "gedit"; see `+"`pvm app catalog`") {
		t.Errorf("unknown app: exit %d, requests %v, stderr %s", code, reqs, errOut)
	}

	code, out = captureStdout(t, func() int { return CmdApp([]string{"catalog"}) })
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 4 || !slices.Equal(strings.Fields(lines[0]), []string{"ID", "NAME", "SOURCE", "WINDOW", "DESCRIPTION"}) ||
		!slices.Equal(strings.Fields(lines[1]), []string{"claude-desktop", "Claude", "Desktop", "official", "yes", "desktop", "app"}) ||
		!slices.Equal(strings.Fields(lines[2]), []string{"codex", "Codex", "CLI", "official", "no", "coding", "agent"}) {
		t.Errorf("catalog: exit %d\n%s", code, out)
	}

	code, out = captureStdout(t, func() int { return CmdApp([]string{"ls"}) })
	var rows [][]string
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		rows = append(rows, strings.Fields(l))
	}
	want := [][]string{{"VM", "ID", "STATE", "RUNS"}, {"dev1", "gedit", "running", "gedit"}, nil, {"VM", "INSTALLED"}, {"dev1", "claude-code,", "claude-desktop"}, {"dev2", "codex"}}
	if code != 0 || len(rows) != len(want) {
		t.Fatalf("ls: exit %d\n%s", code, out)
	}
	for i := range want {
		if !slices.Equal(rows[i], want[i]) {
			t.Errorf("ls row %d = %q, want %q", i, rows[i], want[i])
		}
	}
}
