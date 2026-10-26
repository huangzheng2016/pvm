package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

func TestMountAPIRejectsInvalidProfilesAndOptions(t *testing.T) {
	tests := []struct {
		name string
		body map[string]string
	}{
		{"loose", map[string]string{"profile": "loose"}},
		{"fscache", map[string]string{"profile": "fscache"}},
		{"injected options", map[string]string{"profile": "readahead,cache=loose;id"}},
		{"arbitrary options", map[string]string{"options": "cache=loose"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, spec, host := newMountTestServer(t)
			tc.body["host"] = host
			tc.body["guest"] = "/mnt/data"
			body, err := json.Marshal(tc.body)
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("POST", "/v1/vms/dev1/mounts", bytes.NewReader(body))
			rr := httptest.NewRecorder()
			s.handler().ServeHTTP(rr, req)
			if rr.Code != 400 {
				t.Fatalf("add status = %d: %s", rr.Code, rr.Body.String())
			}
			loaded, err := vm.Load(s.P, spec.Name)
			if err != nil {
				t.Fatal(err)
			}
			if len(loaded.Mounts) != 0 {
				t.Fatalf("mounts after rejected add = %+v", loaded.Mounts)
			}
		})
	}
}

func TestMountListNormalizesProfiles(t *testing.T) {
	s, spec, host := newMountTestServer(t)
	spec.Mounts = []vm.GuestMount{
		{Tag: "pvm0", Host: host, Guest: "/mnt/legacy"},
		{Tag: "pvm1", Host: host, Guest: "/mnt/read", Profile: vm.MountProfileReadahead},
	}
	req := httptest.NewRequest("GET", "/v1/vms/dev1/mounts", nil)
	rr := httptest.NewRecorder()
	s.handler().ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("list status = %d: %s", rr.Code, rr.Body.String())
	}
	var mounts []vm.GuestMount
	if err := json.Unmarshal(rr.Body.Bytes(), &mounts); err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 2 || mounts[0].Profile != vm.MountProfileCoherent || mounts[1].Profile != vm.MountProfileReadahead {
		t.Fatalf("listed profiles = %+v", mounts)
	}
	if spec.Mounts[0].Profile != "" {
		t.Fatalf("list mutated legacy spec: %+v", spec.Mounts[0])
	}
}

func TestNewGuestMountReusesFreeSlot(t *testing.T) {
	_, spec, host := newMountTestServer(t)
	spec.Mounts = []vm.GuestMount{
		{Tag: "pvm0", Host: host, Guest: "/mnt/a"},
		{Tag: "pvm2", Host: host, Guest: "/mnt/b"},
	}
	m, err := newGuestMount(spec, host, "/mnt/c", "")
	if err != nil {
		t.Fatal(err)
	}
	if m.Tag != "pvm1" || m.Profile != vm.MountProfileCoherent {
		t.Fatalf("allocated mount = %+v, want pvm1 coherent", m)
	}
	m, err = newGuestMount(spec, host, "/mnt/read", vm.MountProfileReadahead)
	if err != nil {
		t.Fatal(err)
	}
	if m.Profile != vm.MountProfileReadahead {
		t.Fatalf("readahead profile = %q", m.Profile)
	}
	if _, err := newGuestMount(spec, host, "/mnt/a", ""); err == nil {
		t.Fatal("duplicate guest path must fail")
	}
	if _, err := newGuestMount(spec, host, "/share", ""); err == nil {
		t.Fatal("reserved /share must fail")
	}
}

func TestGuestMountProfilePersists(t *testing.T) {
	s, spec, host := newMountTestServer(t)
	m, err := newGuestMount(spec, host, "/mnt/read", vm.MountProfileReadahead)
	if err != nil {
		t.Fatal(err)
	}
	spec.Mounts = []vm.GuestMount{m}
	if err := spec.Save(s.P); err != nil {
		t.Fatal(err)
	}
	loaded, err := vm.Load(s.P, spec.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Mounts) != 1 || loaded.Mounts[0].Profile != vm.MountProfileReadahead {
		t.Fatalf("loaded mounts = %+v", loaded.Mounts)
	}
}

func TestGuestMountAddScriptProfiles(t *testing.T) {
	for _, profile := range []string{"", vm.MountProfileCoherent, vm.MountProfileReadahead} {
		m := vm.GuestMount{Tag: "pvm0", Host: "/host", Guest: "/mnt/data", Profile: profile}
		script, err := guestMountAddScript(m)
		if err != nil {
			t.Fatal(err)
		}
		options, err := vm.GuestMountOptions(profile)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Count(script, options); got != 2 {
			t.Fatalf("profile %q options occur %d times, want mount and fstab: %s", profile, got, script)
		}
		if !strings.Contains(script, "/sys/bus/virtio/devices/virtio*/mount_tag") {
			t.Fatalf("profile %q does not wait for the hotplugged mount tag: %s", profile, script)
		}
	}
	if _, err := guestMountAddScript(vm.GuestMount{Profile: "cache=loose"}); err == nil {
		t.Fatal("invalid profile must not generate a guest script")
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

func TestPrepareMountSlotsPreflightsAllSlots(t *testing.T) {
	s, spec, host := newMountTestServer(t)
	if err := s.prepareMountSlots(spec); err != nil {
		t.Fatal(err)
	}
	first := s.P.MountSlot(spec.Name, 1)
	before, err := os.Readlink(first)
	if err != nil {
		t.Fatal(err)
	}
	blocked := s.P.MountSlot(spec.Name, 2)
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocked, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	spec.Mounts = []vm.GuestMount{{Tag: "pvm0", Host: host, Guest: "/mnt/data"}}
	if err := s.prepareMountSlots(spec); err == nil {
		t.Fatal("expected slot error")
	}
	if after, err := os.Readlink(first); err != nil || after != before {
		t.Fatalf("earlier slot changed: %q %v", after, err)
	}
	if data, err := os.ReadFile(blocked); err != nil || string(data) != "keep" {
		t.Fatalf("blocked file changed: %q %v", data, err)
	}
}

func mountRollbackQMP(t *testing.T, reject bool) (*qemu.QMP, <-chan struct{}) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "pvm-qmp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "q.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	deleted := make(chan struct{}, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintln(conn, `{"QMP":{"version":{"qemu":{"major":11}},"capabilities":[]}}`)
		scanner := bufio.NewScanner(conn)
		for scanner.Scan() {
			var cmd struct {
				Execute string `json:"execute"`
			}
			if json.Unmarshal(scanner.Bytes(), &cmd) != nil {
				return
			}
			if cmd.Execute == "device_del" {
				deleted <- struct{}{}
				if reject {
					fmt.Fprintln(conn, `{"error":{"class":"GenericError","desc":"device busy"}}`)
				} else {
					fmt.Fprintln(conn, `{"return":{}}`)
					fmt.Fprintln(conn, `{"event":"DEVICE_DELETED","data":{"device":"fs1"}}`)
				}
			} else {
				fmt.Fprintln(conn, `{"return":{}}`)
			}
		}
	}()
	q, err := qemu.DialQMP(socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	return q, deleted
}

func TestMountRollbackRetainsActiveBackendAndRecoverySpec(t *testing.T) {
	for _, failure := range []string{"guest", "device", "none"} {
		t.Run(failure, func(t *testing.T) {
			s, spec, host := newMountTestServer(t)
			m := vm.GuestMount{Tag: "pvm0", Host: host, Guest: "/mnt/data", Profile: vm.MountProfileReadahead}
			next := *spec
			next.Mounts = []vm.GuestMount{m}
			if err := s.prepareMountSlots(&next); err != nil {
				t.Fatal(err)
			}
			q, deleted := mountRollbackQMP(t, failure == "device")
			err := s.rollbackAddedMountWith(spec, m, q, true, func(*vm.Spec, string) (string, error) {
				if failure == "guest" {
					return "", fmt.Errorf("guest busy")
				}
				return "", nil
			})
			got, readErr := os.Readlink(s.P.MountSlot(spec.Name, 1))
			if readErr != nil {
				t.Fatal(readErr)
			}
			loaded, loadErr := vm.Load(s.P, spec.Name)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if failure != "none" {
				if err == nil || !strings.Contains(err.Error(), "pvm mount rm dev1 /mnt/data") {
					t.Fatalf("missing recovery: %v", err)
				}
				if got != host || len(loaded.Mounts) != 1 || loaded.Mounts[0] != m || len(spec.Mounts) != 1 {
					t.Fatalf("active backend/state lost: %s %+v", got, loaded.Mounts)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if got == host || len(loaded.Mounts) != 0 || len(spec.Mounts) != 0 {
					t.Fatalf("successful rollback incomplete: %s %+v", got, loaded.Mounts)
				}
			}
			if failure == "guest" {
				select {
				case <-deleted:
					t.Fatal("attempted detach after failed unmount")
				default:
				}
			} else {
				select {
				case <-deleted:
				default:
					t.Fatal("device not detached")
				}
			}
		})
	}
}

func TestFailedMountRecoverySaveKeepsSlotReservedInMemory(t *testing.T) {
	s, spec, host := newMountTestServer(t)
	m := vm.GuestMount{Tag: "pvm0", Host: host, Guest: "/mnt/data"}
	next := *spec
	next.Mounts = []vm.GuestMount{m}
	if err := s.prepareMountSlots(&next); err != nil {
		t.Fatal(err)
	}
	// A directory at the spec path forces AtomicWrite's final rename to fail.
	specPath := spec.SpecPath(s.P)
	if err := os.Remove(specPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(specPath, 0700); err != nil {
		t.Fatal(err)
	}
	err := s.retainFailedMount(spec, m, fmt.Errorf("device busy"))
	if err == nil || !strings.Contains(err.Error(), "stop the VM") {
		t.Fatalf("missing recovery: %v", err)
	}
	if len(spec.Mounts) != 1 {
		t.Fatal("active slot not reserved in memory")
	}
	another, err := newGuestMount(spec, host, "/mnt/other", "")
	if err != nil {
		t.Fatal(err)
	}
	if another.Tag == m.Tag {
		t.Fatal("failed recovery reused active backend")
	}
	if got, err := os.Readlink(s.P.MountSlot(spec.Name, 1)); err != nil || got != host {
		t.Fatalf("active backend changed: %s %v", got, err)
	}
}
