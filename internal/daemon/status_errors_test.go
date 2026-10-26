package daemon

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	srv := NewServer(p, &config.Config{}, &qemu.Toolchain{}, qemu.AccelTCG, "", nil)
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
	srv := NewServer(p, &config.Config{}, &qemu.Toolchain{}, qemu.AccelTCG, "", nil)
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

func TestManagedSSHUsesOnlyDedicatedIdentity(t *testing.T) {
	p := paths.New(t.TempDir())
	srv := NewServer(p, &config.Config{}, &qemu.Toolchain{}, qemu.AccelTCG, "", nil)
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
	srv := NewServer(p, &config.Config{}, &qemu.Toolchain{}, qemu.AccelTCG, "", nil)
	out := httptest.NewRecorder()
	srv.handler().ServeHTTP(out, httptest.NewRequest("POST", "/v1/vms/dev1/snapshot", nil))
	if out.Code != 409 {
		t.Fatalf("snapshot: %d %s", out.Code, out.Body.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("snapshot rejection changed guest mounts")
	}
}
