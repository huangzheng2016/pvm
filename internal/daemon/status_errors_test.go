package daemon

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/huangzheng2016/pvm/internal/config"
	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/qemu"
	"github.com/huangzheng2016/pvm/internal/vm"
)

func TestVMListAndStatusReportDamagedSpec(t *testing.T) {
	p := paths.New(t.TempDir())
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	dir := p.VMDir("dev1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vm.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(p, &config.Config{}, &qemu.Toolchain{}, qemu.AccelTCG, nil)
	for _, path := range []string{"/v1/vms", "/v1/status"} {
		out := httptest.NewRecorder()
		srv.handler().ServeHTTP(out, httptest.NewRequest("GET", path, nil))
		if out.Code != 500 {
			t.Fatalf("%s: got %d", path, out.Code)
		}
	}
}

func TestStoppedVMDoesNotRetainReadyStatus(t *testing.T) {
	p := paths.New(t.TempDir())
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	dir := p.VMDir("dev1")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	spec := &vm.Spec{Name: "dev1", User: "dev", Role: "user"}
	if err := spec.Save(p); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(p, &config.Config{}, &qemu.Toolchain{}, qemu.AccelTCG, nil)
	srv.stage("dev1", "ready", "")
	out := httptest.NewRecorder()
	srv.handler().ServeHTTP(out, httptest.NewRequest("GET", "/v1/vms/dev1", nil))
	if out.Code != 200 {
		t.Fatalf("get: %d %s", out.Code, out.Body.String())
	}
	var info vmInfo
	if err := json.Unmarshal(out.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	if info.Ready || info.Running {
		t.Fatal("stopped VM reported ready")
	}
}

func TestVMListReportsReadiness(t *testing.T) {
	p := paths.New(t.TempDir())
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(p, &config.Config{}, &qemu.Toolchain{}, qemu.AccelTCG, nil)
	for _, name := range []string{"ready1", "failed1", "starting1", "recovered1", "reset1", "stopped1"} {
		if err := os.MkdirAll(p.VMDir(name), 0700); err != nil {
			t.Fatal(err)
		}
		spec := &vm.Spec{Name: name, User: "dev", Role: "user", IdentityReset: name == "reset1"}
		if err := spec.Save(p); err != nil {
			t.Fatal(err)
		}
		if name != "stopped1" {
			markVMRunning(t, srv, name)
		}
	}
	for _, name := range []string{"ready1", "reset1", "stopped1"} {
		srv.stage(name, "agent", "guest agent installed")
		srv.stage(name, "ready", "all ready")
	}
	srv.stage("failed1", "provision", "waiting for ssh")
	srv.stage("failed1", "failed", "agent installation failed; run `pvm stop failed1 && pvm start failed1`")
	srv.stage("starting1", "boot", "qemu pid up")
	srv.stage("starting1", "provision", "waiting for ssh")

	out := httptest.NewRecorder()
	srv.handler().ServeHTTP(out, httptest.NewRequest("GET", "/v1/vms", nil))
	if out.Code != 200 {
		t.Fatalf("list: %d %s", out.Code, out.Body.String())
	}
	var infos []vmInfo
	if err := json.Unmarshal(out.Body.Bytes(), &infos); err != nil {
		t.Fatal(err)
	}
	type row struct {
		running, ready bool
		stage, detail  string
	}
	got := map[string]row{}
	for _, info := range infos {
		if info.Stages != nil {
			t.Errorf("%s: list carries the full stages array", info.Spec.Name)
		}
		r := row{running: info.Running, ready: info.Ready}
		if info.Stage != nil {
			r.stage, r.detail = info.Stage.Stage, info.Stage.Detail
		}
		got[info.Spec.Name] = r
	}
	want := map[string]row{
		"ready1":     {true, true, "ready", "all ready"},
		"failed1":    {true, false, "failed", "agent installation failed; run `pvm stop failed1 && pvm start failed1`"},
		"starting1":  {true, false, "provision", "waiting for ssh"},
		"recovered1": {true, false, "", ""},
		"reset1":     {true, false, "ready", "all ready"},
		"stopped1":   {false, false, "ready", "all ready"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("list:\n got %+v\nwant %+v", got, want)
	}
	if strings.Contains(out.Body.String(), `"at"`) {
		t.Fatalf("list stage carries a timestamp: %s", out.Body.String())
	}

	out = httptest.NewRecorder()
	srv.handler().ServeHTTP(out, httptest.NewRequest("GET", "/v1/vms/failed1", nil))
	var one vmInfo
	if err := json.Unmarshal(out.Body.Bytes(), &one); err != nil {
		t.Fatal(err)
	}
	if one.Ready || len(one.Stages) != 2 || one.Stage == nil || one.Stage.Stage != "failed" {
		t.Fatalf("get: %s", out.Body.String())
	}
}

func TestManagedSSHUsesOnlyDedicatedIdentity(t *testing.T) {
	p := paths.New(t.TempDir())
	srv := NewServer(p, &config.Config{}, &qemu.Toolchain{}, qemu.AccelTCG, nil)
	args, _, err := srv.sshBaseArgs(&vm.Spec{Name: "dev1", User: "dev", Role: "gateway", SSHPort: 10022})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	for _, option := range []string{"-F /dev/null", "IdentitiesOnly=yes", "IdentityAgent=none", "ForwardAgent=no"} {
		if !strings.Contains(joined, option) {
			t.Fatalf("missing SSH isolation option %s", option)
		}
	}
}

func TestManagedSSHConnectTimeoutAllowsTCG(t *testing.T) {
	p := paths.New(t.TempDir())
	srv := NewServer(p, &config.Config{}, &qemu.Toolchain{}, qemu.AccelTCG, nil)
	args, _, err := srv.sshBaseArgs(&vm.Spec{Name: "dev1", User: "dev", Role: "gateway", SSHPort: 10022})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(args, " "), "ConnectTimeout=10") {
		t.Fatalf("managed SSH timeout does not match the CLI: %v", args)
	}
}

func TestSnapshotExistingArtifactsRejectedBeforeGuestChanges(t *testing.T) {
	p := paths.New(t.TempDir())
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.VMDir("dev1"), 0700); err != nil {
		t.Fatal(err)
	}
	spec := &vm.Spec{Name: "dev1", User: "dev", Role: "user"}
	if err := spec.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(qemu.SnapPaths(p.VMDir("dev1")).Manifest, []byte("existing snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	marker := filepath.Join(bin, "ssh-called")
	if err := os.WriteFile(filepath.Join(bin, "ssh"), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	srv := NewServer(p, &config.Config{}, &qemu.Toolchain{}, qemu.AccelTCG, nil)
	out := httptest.NewRecorder()
	srv.handler().ServeHTTP(out, httptest.NewRequest("POST", "/v1/vms/dev1/snapshot", nil))
	if out.Code != 409 {
		t.Fatalf("snapshot: %d %s", out.Code, out.Body.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("snapshot rejection changed guest mounts")
	}
}

func TestHandleGetDoesNotScanUnrelatedCorruptVM(t *testing.T) {
	p := paths.New(t.TempDir())
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.VMDir("dev1"), 0700); err != nil {
		t.Fatal(err)
	}
	spec := &vm.Spec{Name: "dev1", User: "dev", Role: "user"}
	if err := spec.Save(p); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.VMDir("broken"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.VMDir("broken"), "vm.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(p, &config.Config{}, &qemu.Toolchain{}, qemu.AccelTCG, nil)
	out := httptest.NewRecorder()
	srv.handler().ServeHTTP(out, httptest.NewRequest("GET", "/v1/vms/dev1", nil))
	if out.Code != 200 {
		t.Fatalf("handleGet failed on unrelated corrupt VM: %d %s", out.Code, out.Body.String())
	}
}
