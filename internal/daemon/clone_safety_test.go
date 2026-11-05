package daemon

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/vm"
)

func TestClonePreservesUnrecognizedDestination(t *testing.T) {
	s, src, _ := newMapTestServer(t)
	dest := s.P.VMDir("dev3")
	if err := os.MkdirAll(dest, 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dest, "user-data")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/v1/vms/dev1/clone", strings.NewReader(`{"new_name":"dev3"}`))
	r.SetPathValue("name", src.Name)
	w := httptest.NewRecorder()
	s.handleClone(w, r)
	if w.Code != 409 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "keep" {
		t.Fatalf("existing data changed: %q %v", data, err)
	}
	if _, ok := s.Clusters.Get(src.ClusterID).VMs["dev3"]; ok {
		t.Fatal("conflicting clone changed membership")
	}
}

func TestRemoveVMResourcesPreservesStateOnSaveFailure(t *testing.T) {
	s, spec, _ := newMapTestServer(t)
	statePath := filepath.Join(s.P.Home, "clusters", "clusters.json")
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.removeVMMember(spec.ClusterID, spec.Name); err == nil {
		t.Fatal("membership save error hidden")
	}
	if err := s.closeVMResources(spec.Name, s.Clusters.Get(spec.ClusterID)); err == nil {
		t.Fatal("resource cleanup save error hidden")
	}
	if _, ok := s.Clusters.Get(spec.ClusterID).VMs[spec.Name]; !ok {
		t.Fatal("membership was removed after failed save")
	}
	if _, err := vm.Load(s.P, spec.Name); err != nil {
		t.Fatalf("VM files changed: %v", err)
	}
}

func TestCompleteIdentityResetPreservesLatestSpec(t *testing.T) {
	s, spec, _ := newMapTestServer(t)
	spec.CreatedAt = time.Now()
	spec.IdentityReset = true
	if err := spec.Save(s.P); err != nil {
		t.Fatal(err)
	}
	old := *spec
	fresh := *spec
	fresh.DiskGB = 60
	fresh.Mounts = []vm.GuestMount{{Host: "/mock/share", Guest: "/data", Tag: "pvm0"}}
	if err := fresh.Save(s.P); err != nil {
		t.Fatal(err)
	}
	if err := s.completeIdentityReset(&old); err != nil {
		t.Fatal(err)
	}
	got, err := vm.Load(s.P, spec.Name)
	if err != nil {
		t.Fatal(err)
	}
	if got.IdentityReset || got.DiskGB != 60 || len(got.Mounts) != 1 {
		t.Fatalf("late completion overwrote state: %+v", got)
	}
	if got.SSHKeyName != got.Name {
		t.Fatal("clone did not switch to its own SSH key")
	}
}

func TestCompleteIdentityResetRejectsDeletedOrReplacedVM(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "deleted", true: "replaced"}[replace], func(t *testing.T) {
			s, spec, _ := newMapTestServer(t)
			spec.CreatedAt = time.Now()
			spec.IdentityReset = true
			if err := spec.Save(s.P); err != nil {
				t.Fatal(err)
			}
			if err := vm.Delete(s.P, spec.Name); err != nil {
				t.Fatal(err)
			}
			if replace {
				fresh := *spec
				fresh.CreatedAt = fresh.CreatedAt.Add(time.Second)
				if err := os.MkdirAll(s.P.VMDir(spec.Name), 0700); err != nil {
					t.Fatal(err)
				}
				if err := fresh.Save(s.P); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.completeIdentityReset(spec); err == nil {
				t.Fatal("stale completion accepted")
			}
			if replace {
				got, err := vm.Load(s.P, spec.Name)
				if err != nil || !got.IdentityReset {
					t.Fatalf("replacement changed: %+v %v", got, err)
				}
			} else if _, err := os.Stat(s.P.VMDir(spec.Name)); !os.IsNotExist(err) {
				t.Fatalf("deleted VM recreated: %v", err)
			}
		})
	}
}

func TestCloneAuthorizedKeysReplaceOnlyManagedKey(t *testing.T) {
	dir := t.TempDir()
	auth := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(auth, []byte("ssh-ed25519 OLD pvm\nssh-ed25519 USER keep\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// macOS lacks GNU chown --reference; ownership behavior belongs to the
	// Linux guest, while this test executes the actual rewrite and rename.
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "chown"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	script := cloneAuthorizedKeyScript("dev1", "ssh-ed25519 OLD pvm", "ssh-ed25519 NEW pvm")
	script = strings.ReplaceAll(script, "/home/dev1/.ssh/authorized_keys", auth)
	for i := 0; i < 2; i++ {
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append(os.Environ(), "SUDO=", "PATH="+bin+":"+os.Getenv("PATH"))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("key rewrite: %v: %s", err, out)
		}
	}
	data, err := os.ReadFile(auth)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "ssh-ed25519 USER keep\nssh-ed25519 NEW pvm\n" {
		t.Fatalf("wrong authorized keys: %q", data)
	}
	info, err := os.Stat(auth)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("key permissions=%o", info.Mode().Perm())
	}
}

func TestClonePinsSourceEffectiveExit(t *testing.T) {
	for _, tc := range []struct{ name, override, defaultExit, want string }{
		{"vm proxy", "my-exit", "other-exit", "my-exit"},
		{"explicit direct", "direct", "my-exit", "direct"},
		{"inherited proxy", "", "my-exit", "my-exit"},
		{"inherited direct", "", "", "direct"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, src, _ := newMapTestServer(t)
			if err := s.Clusters.SetDefaultExit(src.ClusterID, tc.defaultExit); err != nil {
				t.Fatal(err)
			}
			if err := s.Clusters.SetVMExit(src.ClusterID, src.Name, tc.override); err != nil {
				t.Fatal(err)
			}
			if _, _, err := s.prepareCloneNetwork(src, "dev3"); err != nil {
				t.Fatal(err)
			}
			if err := s.Clusters.SetDefaultExit(src.ClusterID, "changed-exit"); err != nil {
				t.Fatal(err)
			}
			if got := s.Clusters.Get(src.ClusterID).VMExits["dev3"]; got != tc.want {
				t.Fatalf("clone route=%q want %q", got, tc.want)
			}
		})
	}
}

func TestCloneNetworkPreservesExistingMember(t *testing.T) {
	s, src, existing := newMapTestServer(t)
	if err := s.Clusters.SetVMExit(existing.ClusterID, existing.Name, "my-exit"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.prepareCloneNetwork(src, existing.Name); err == nil {
		t.Fatal("existing membership claimed")
	}
	cl := s.Clusters.Get(existing.ClusterID)
	if cl.VMs[existing.Name] == nil || cl.VMExits[existing.Name] != "my-exit" {
		t.Fatal("existing membership changed")
	}
}
