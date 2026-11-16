package cli

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/huangzheng2016/pvm/internal/daemon"
	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/workspace"
)

func TestRelInside(t *testing.T) {
	sep := string(filepath.Separator)
	root := filepath.Join(sep+"src", "web")
	for _, tc := range []struct {
		p    string
		want string
		ok   bool
	}{
		{root, "", true},
		{root + sep, "", true},
		{filepath.Join(root, "a", "b"), "a/b", true},
		{filepath.Join(root, "..", "web2"), "", false},
		{root + "2", "", false},
		{filepath.Join(sep + "src"), "", false},
		{sep, "", false},
	} {
		got, ok := relInside(root, tc.p)
		if ok != tc.ok || got != tc.want {
			t.Errorf("relInside(%q, %q) = %q, %v; want %q, %v", root, tc.p, got, ok, tc.want, tc.ok)
		}
	}
	if runtime.GOOS == "windows" {
		// drive letters and folder names compare case-insensitively, and
		// the result keeps the cwd's spelling
		got, ok := relInside(`C:\Src\Web`, `c:\src\web\Api\v1`)
		if !ok || got != "Api/v1" {
			t.Errorf("windows case: %q, %v", got, ok)
		}
	}
}

func TestGuestDir(t *testing.T) {
	ws := &workspace.Workspace{GuestPath: testGP}
	if d, ok := guestDir(ws, ""); !ok || d != testGP {
		t.Errorf("root: %q %v", d, ok)
	}
	if d, ok := guestDir(ws, "src/app"); !ok || d != testGP+"/src/app" {
		t.Errorf("subdir: %q %v", d, ok)
	}
	if _, ok := guestDir(nil, "x"); ok {
		t.Error("no workspace must not give a dir")
	}
}

// fakeVMDaemon serves /v1/vms for the given user VMs plus one gateway.
func fakeVMDaemon(t *testing.T, vms ...string) (*paths.Paths, *daemon.Client) {
	t.Helper()
	home, err := os.MkdirTemp("", "pvm-defvm-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("PVM_HOME", home)
	p := paths.New(home)
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	var ln net.Listener
	if runtime.GOOS == "windows" {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err == nil {
			err = os.WriteFile(p.DaemonAddr(), []byte(ln.Addr().String()), 0o600)
		}
	} else {
		ln, err = net.Listen("unix", p.DaemonSock())
	}
	if err != nil {
		t.Fatal(err)
	}
	view := func(name, role string) string {
		return fmt.Sprintf(`{"running":true,"spec":{"name":%q,"role":%q}}`, name, role)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/vms" {
			items := []string{view("pvm-gw1", "gateway")}
			for _, n := range vms {
				items = append(items, view(n, "user"))
			}
			fmt.Fprintf(w, "[%s]", strings.Join(items, ","))
			return
		}
		for _, n := range vms {
			if r.URL.Path == "/v1/vms/"+n {
				fmt.Fprint(w, view(n, "user"))
				return
			}
		}
		http.Error(w, `{"error":"no such vm"}`, http.StatusNotFound)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return p, daemon.NewClient(p)
}

func saveWorkspace(t *testing.T, p *paths.Paths, id, vm, host string) {
	t.Helper()
	ws := &workspace.Workspace{ID: id, VM: vm, Mode: workspace.ModeMount, HostPath: host, GuestPath: workspace.GuestPathFor(id)}
	if err := workspace.NewStore(p).Save(ws); err != nil {
		t.Fatal(err)
	}
}

func TestResolveVM(t *testing.T) {
	t.Run("explicit name wins", func(t *testing.T) {
		p, c := fakeVMDaemon(t, "dev1", "dev2")
		name, ws, _, err := resolveVM(c, p, "dev2")
		if err != nil || name != "dev2" || ws != nil {
			t.Fatalf("got %q %v %v", name, ws, err)
		}
	})
	t.Run("workspace of the cwd", func(t *testing.T) {
		p, c := fakeVMDaemon(t, "dev1", "dev2")
		outer := t.TempDir()
		inner := filepath.Join(outer, "api")
		if err := os.MkdirAll(filepath.Join(inner, "src"), 0o700); err != nil {
			t.Fatal(err)
		}
		saveWorkspace(t, p, "outer", "dev1", outer)
		saveWorkspace(t, p, "inner", "dev2", inner)
		t.Chdir(filepath.Join(inner, "src"))
		name, ws, rel, err := resolveVM(c, p, "")
		if err != nil || name != "dev2" || ws == nil || ws.ID != "inner" || rel != "src" {
			t.Fatalf("got %q %v %q %v", name, ws, rel, err)
		}
		// an explicit name for the workspace's own vm keeps the directory
		if _, ws, rel, _ := resolveVM(c, p, "dev2"); ws == nil || rel != "src" {
			t.Fatalf("explicit same vm lost the workspace: %v %q", ws, rel)
		}
		// another vm does not start in a folder it does not have
		if _, ws, _, _ := resolveVM(c, p, "dev1"); ws != nil {
			t.Fatalf("explicit other vm kept workspace %s", ws.ID)
		}
	})
	t.Run("workspace vm deleted", func(t *testing.T) {
		p, c := fakeVMDaemon(t, "dev1")
		dir := t.TempDir()
		saveWorkspace(t, p, "web", "gone", dir)
		t.Chdir(dir)
		if _, _, _, err := resolveVM(c, p, ""); err == nil || !strings.Contains(err.Error(), "workspace web belongs to vm gone") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("only user vm", func(t *testing.T) {
		p, c := fakeVMDaemon(t, "dev1")
		t.Chdir(t.TempDir())
		name, ws, _, err := resolveVM(c, p, "")
		if err != nil || name != "dev1" || ws != nil {
			t.Fatalf("got %q %v %v", name, ws, err)
		}
	})
	t.Run("several vms", func(t *testing.T) {
		p, c := fakeVMDaemon(t, "dev2", "dev1")
		t.Chdir(t.TempDir())
		_, _, _, err := resolveVM(c, p, "")
		if err == nil || !strings.Contains(err.Error(), "name one: dev1, dev2") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("no vm", func(t *testing.T) {
		p, c := fakeVMDaemon(t)
		t.Chdir(t.TempDir())
		if _, _, _, err := resolveVM(c, p, ""); err == nil || !strings.Contains(err.Error(), "pvm create") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestSplitVMArg(t *testing.T) {
	_, c := fakeVMDaemon(t, "dev1")
	for _, tc := range []struct {
		args    []string
		vm      string
		command []string
	}{
		{nil, "", nil},
		{[]string{"dev1"}, "dev1", []string{}},
		{[]string{"dev1", "ls", "-l"}, "dev1", []string{"ls", "-l"}},
		{[]string{"ls", "-l"}, "", []string{"ls", "-l"}},
		{[]string{"--", "dev1"}, "", []string{"dev1"}},
	} {
		vm, cmd, err := splitVMArg(c, tc.args)
		if err != nil || vm != tc.vm || strings.Join(cmd, " ") != strings.Join(tc.command, " ") || len(cmd) != len(tc.command) {
			t.Errorf("splitVMArg(%q) = %q, %q, %v", tc.args, vm, cmd, err)
		}
	}
}

func TestConfirmYes(t *testing.T) {
	for in, want := range map[string]bool{"\n": true, "": true, "y\n": true, "yes\n": true, "n\n": false, "No\n": false} {
		if got := confirmYes(strings.NewReader(in), new(strings.Builder), "start?"); got != want {
			t.Errorf("confirmYes(%q) = %v", in, got)
		}
	}
}
