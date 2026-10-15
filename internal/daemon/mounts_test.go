package daemon

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/huangzheng2016/pvm/internal/config"
	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/qemu"
	"github.com/huangzheng2016/pvm/internal/vm"
)

func newMountTestServer(t *testing.T) (*Server, *vm.Spec, string) {
	t.Helper()
	p := paths.New(t.TempDir())
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	spec := &vm.Spec{Name: "dev1", Role: "user", User: "dev", CPUs: 1, MemMB: 128}
	if err := os.MkdirAll(p.VMDir(spec.Name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := spec.Save(p); err != nil {
		t.Fatal(err)
	}
	s := NewServer(p, &config.Config{}, nil, qemu.AccelTCG, "", nil)
	s.vms[spec.Name] = spec
	return s, spec, t.TempDir()
}

func TestMountAPIRequiresRunningVM(t *testing.T) {
	s, spec, host := newMountTestServer(t)
	body, err := json.Marshal(map[string]string{"host": host, "guest": "/home/dev/proj"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/v1/vms/dev1/mounts", bytes.NewReader(body))
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, req)
	if rr.Code != 500 {
		t.Fatalf("add status = %d: %s", rr.Code, rr.Body.String())
	}
	loaded, err := vm.Load(s.P, spec.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Mounts) != 0 {
		t.Fatalf("mounts after rejected add = %+v", loaded.Mounts)
	}

	spec.Mounts = []vm.GuestMount{{Tag: "pvm0", Host: host, Guest: "/home/dev/proj"}}
	if err := spec.Save(s.P); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest("DELETE", "/v1/vms/dev1/mounts?guest="+url.QueryEscape("/home/dev/proj"), nil)
	rr = httptest.NewRecorder()
	s.handler().ServeHTTP(rr, req)
	if rr.Code != 500 {
		t.Fatalf("remove status = %d: %s", rr.Code, rr.Body.String())
	}
	loaded, err = vm.Load(s.P, spec.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Mounts) != 1 {
		t.Fatalf("mounts after rejected remove = %+v", loaded.Mounts)
	}
}

func TestPrepareMountSlots(t *testing.T) {
	s, spec, host := newMountTestServer(t)
	spec.Mounts = []vm.GuestMount{{Tag: "pvm0", Host: host, Guest: "/home/dev/proj"}}
	if err := s.prepareMountSlots(spec); err != nil {
		t.Fatal(err)
	}
	if got, err := os.Readlink(s.P.MountSlot(spec.Name, 1)); err != nil || got != host {
		t.Fatalf("slot target = %q, %v; want %q", got, err, host)
	}
	spec.Mounts = nil
	if err := s.prepareMountSlots(spec); err != nil {
		t.Fatal(err)
	}
	wantEmpty := filepath.Join(s.P.MountDir(spec.Name), "empty")
	if got, err := os.Readlink(s.P.MountSlot(spec.Name, 1)); err != nil || got != wantEmpty {
		t.Fatalf("cleared slot target = %q, %v; want %q", got, err, wantEmpty)
	}
}

func TestNewGuestMountReusesFreeSlot(t *testing.T) {
	_, spec, host := newMountTestServer(t)
	spec.Mounts = []vm.GuestMount{
		{Tag: "pvm0", Host: host, Guest: "/mnt/a"},
		{Tag: "pvm2", Host: host, Guest: "/mnt/b"},
	}
	m, err := newGuestMount(spec, host, "/mnt/c")
	if err != nil {
		t.Fatal(err)
	}
	if m.Tag != "pvm1" {
		t.Fatalf("allocated tag = %q, want pvm1", m.Tag)
	}
	if _, err := newGuestMount(spec, host, "/mnt/a"); err == nil {
		t.Fatal("duplicate guest path must fail")
	}
	if _, err := newGuestMount(spec, host, "/share"); err == nil {
		t.Fatal("reserved /share must fail")
	}
}

func TestDynamicMountSlotReady(t *testing.T) {
	args := []string{
		"qemu", "-fsdev", "local,id=fs1,path=/m/slot1,security_model=none",
		"-device", "pcie-root-port,id=rp1,bus=pcie.0,addr=0x11,chassis=129",
		"-fsdev", "local,id=fs10,path=/m/slot10,security_model=none",
	}
	if !dynamicMountSlotReady(args, 1) {
		t.Fatal("reserved slot must be detected")
	}
	if dynamicMountSlotReady(args, 2) {
		t.Fatal("missing backend must not be detected")
	}
}

func TestEnsureMountSymlinkRefusesRegularFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slot1")
	if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureMountSymlink(path, t.TempDir()); err == nil {
		t.Fatal("regular file must not be replaced")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "keep" {
		t.Fatalf("regular file changed: %q, %v", data, err)
	}
}
