package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/huangzheng2016/pvm/internal/paths"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huangzheng2016/pvm/internal/vm"
)

func TestParseMountAddArgs(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantProfile string
	}{
		{"default", []string{"dev1", "/host:/guest"}, vm.MountProfileCoherent},
		{"coherent", []string{"dev1", "--profile", "coherent", "/host:/guest"}, vm.MountProfileCoherent},
		{"readahead", []string{"dev1", "--profile", "readahead", "/host:/guest"}, vm.MountProfileReadahead},
		{"equals", []string{"dev1", "--profile=readahead", "/host:/guest"}, vm.MountProfileReadahead},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseMountAddArgs(tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if got.vmName != "dev1" || got.host != "/host" || got.guest != "/guest" || got.profile != tc.wantProfile {
				t.Fatalf("parsed mount = %+v", got)
			}
		})
	}
}

func TestParseMountAddArgsRejectsInvalidInput(t *testing.T) {
	tests := [][]string{
		{"dev1", "--profile", "loose", "/host:/guest"},
		{"dev1", "--profile", "fscache", "/host:/guest"},
		{"dev1", "--profile", "readahead;id", "/host:/guest"},
		{"dev1", "--options", "cache=loose", "/host:/guest"},
		{"dev1", "--profile", "/host:/guest"},
		{"dev1", "--profile", "readahead"},
		{"dev1", "not-a-mount"},
		{"dev1", "/host:/guest", "extra"},
	}
	for _, args := range tests {
		if _, err := parseMountAddArgs(args); err == nil {
			t.Errorf("parseMountAddArgs(%q) unexpectedly passed", args)
		}
	}
}

func TestWriteMountTable(t *testing.T) {
	var out bytes.Buffer
	mounts := []mountView{
		{Host: "/host/a", Guest: "/mnt/legacy"},
		{Host: "/host/b", Guest: "/mnt/read", Profile: vm.MountProfileReadahead},
	}
	if err := writeMountTable(&out, mounts); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("mount table = %q", out.String())
	}
	want := [][]string{
		{"HOST", "GUEST", "PROFILE"},
		{"/host/a", "/mnt/legacy", "coherent"},
		{"/host/b", "/mnt/read", "readahead"},
	}
	for i := range want {
		got := strings.Fields(lines[i])
		if strings.Join(got, " ") != strings.Join(want[i], " ") {
			t.Errorf("line %d = %q, want %q", i, got, want[i])
		}
	}
	if err := writeMountTable(&out, []mountView{{Profile: "loose"}}); err == nil {
		t.Fatal("invalid stored profile must be visible as an error")
	}
}

func TestMountSendsHostPathResolvedInClientDirectory(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "pvm-mount-cli-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("PVM_HOME", filepath.Join(dir, "state"))
	p := paths.New(os.Getenv("PVM_HOME"))
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", p.DaemonSock())
	if err != nil {
		t.Fatal(err)
	}
	received := make(chan string, 1)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			fmt.Fprint(w, `{"ready":true}`)
			return
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		received <- body["host"]
		_ = json.NewEncoder(w).Encode(mountView{Host: body["host"], Guest: body["guest"]})
	})}
	defer server.Close()
	go server.Serve(ln)
	cwd := filepath.Join(dir, "client")
	if err := os.Mkdir(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	if code := CmdMount([]string{"dev1", "./project:/mnt/data"}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if host := <-received; host != filepath.Join(cwd, "project") {
		t.Fatalf("request host = %q", host)
	}
}
