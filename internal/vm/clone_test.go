package vm

import (
	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/qemu"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCloneSpec(t *testing.T) {
	src := &Spec{
		Name: "c1", Image: "ubuntu:24.04", Role: "user", User: "tester",
		GuestUID: 501, CPUs: 2, MemMB: 4096, DiskGB: 20,
		BaseDisk: "/img/golden.qcow2", Golden: 3, ClusterID: 1,
		IP: "192.168.1.10", MAC: "52:54:00:00:00:0a",
		Mounts: []GuestMount{{Tag: "pvm0", Host: "/h", Guest: "/g", Profile: MountProfileReadahead}},
		TZ:     "America/Los_Angeles", Locale: "en_US.UTF-8",
		GitEmail: "a@b.c", GitName: "n", SmbiosSpoof: true,
		PrevTZ: "UTC", PrevLocale: "C.UTF-8",
	}
	dst := CloneSpec(src, "c2", "192.168.1.11", "52:54:00:00:00:0b")

	if dst.Name != "c2" || dst.IP != "192.168.1.11" || dst.MAC != "52:54:00:00:00:0b" {
		t.Errorf("identity fields not replaced: %+v", dst)
	}
	if !dst.IdentityReset {
		t.Error("IdentityReset must be set on a clone")
	}
	if dst.PrevTZ != "" || dst.PrevLocale != "" {
		t.Errorf("route-restore state must not be inherited: %+v", dst)
	}
	if dst.User != "tester" || dst.CPUs != 2 || dst.MemMB != 4096 || dst.DiskGB != 20 {
		t.Errorf("hardware/user not inherited: %+v", dst)
	}
	if dst.TZ != "America/Los_Angeles" || dst.Locale != "en_US.UTF-8" || !dst.SmbiosSpoof {
		t.Errorf("region profile not inherited: %+v", dst)
	}
	if dst.GitEmail != "a@b.c" || dst.GitName != "n" {
		t.Errorf("git identity not inherited: %+v", dst)
	}
	if len(dst.Mounts) != 1 || dst.Mounts[0].Host != "/h" || dst.Mounts[0].Profile != MountProfileReadahead {
		t.Errorf("mounts not inherited: %+v", dst)
	}
	if dst.Golden != 3 || dst.BaseDisk != "/img/golden.qcow2" {
		t.Errorf("image provenance not inherited: %+v", dst)
	}
	// Mutating the clone's mounts must not touch the source's.
	dst.Mounts[0].Host = "/changed"
	if src.Mounts[0].Host != "/h" {
		t.Error("CloneSpec shares the mounts slice with the source")
	}
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"dev1", "a", "vm-test_1"} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("ValidateName(%q) unexpectedly failed: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "1dev", "-x", "a b", "a.b", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("ValidateName(%q) unexpectedly passed", bad)
		}
	}
}

func TestCloneDiskUsesActiveRestoreDisk(t *testing.T) {
	img, err := exec.LookPath("qemu-img")
	if err != nil {
		t.Skip("qemu-img unavailable")
	}
	diskIO, err := exec.LookPath("qemu-io")
	if err != nil {
		t.Skip("qemu-io unavailable")
	}
	p := paths.New(t.TempDir())
	src, dst := &Spec{Name: "dev1"}, &Spec{Name: "dev2"}
	if err := os.MkdirAll(p.VMDir(src.Name), 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", name, err, out)
		}
	}
	run(img, "create", "-f", "qcow2", src.Disk(p), "16M")
	snap := qemu.SnapPaths(p.VMDir(src.Name))
	run(img, "convert", "-f", "qcow2", "-O", "qcow2", src.Disk(p), snap.Snap)
	run(img, "create", "-f", "qcow2", "-b", snap.Snap, "-F", "qcow2", snap.Restore)
	run(diskIO, "-f", "qcow2", "-c", "write -P 0x5a 0 4k", snap.Restore)
	if err := os.WriteFile(src.Seed(p), []byte("seed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CloneDisk(p, &qemu.Toolchain{Img: img}, src, dst); err != nil {
		t.Fatal(err)
	}
	run(diskIO, "-f", "qcow2", "-c", "read -P 0x5a 0 4k", dst.Disk(p))
	run(img, "compare", "-f", "qcow2", "-F", "qcow2", snap.Restore, dst.Disk(p))
	if _, err := os.Stat(filepath.Join(p.VMDir(dst.Name), "seed.img")); err != nil {
		t.Fatal(err)
	}
}
