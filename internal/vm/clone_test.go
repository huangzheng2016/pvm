package vm

import (
	"testing"
)

func TestCloneSpec(t *testing.T) {
	src := &Spec{
		Name: "c1", Image: "ubuntu:24.04", Role: "user", User: "tester",
		GuestUID: 501, Arch: "arm64", CPUs: 2, MemMB: 4096, DiskGB: 20,
		BaseDisk: "/img/golden.qcow2", Golden: 3, ClusterID: 1,
		IP: "192.168.1.10", MAC: "52:54:00:00:00:0a",
		Mounts: []GuestMount{{Tag: "pvm0", Host: "/h", Guest: "/g"}},
		TZ: "America/Los_Angeles", Locale: "en_US.UTF-8",
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
	// Inherited fields.
	if dst.User != "tester" || dst.CPUs != 2 || dst.MemMB != 4096 || dst.DiskGB != 20 {
		t.Errorf("hardware/user not inherited: %+v", dst)
	}
	if dst.TZ != "America/Los_Angeles" || dst.Locale != "en_US.UTF-8" || !dst.SmbiosSpoof {
		t.Errorf("region profile not inherited: %+v", dst)
	}
	if dst.GitEmail != "a@b.c" || dst.GitName != "n" {
		t.Errorf("git identity not inherited: %+v", dst)
	}
	if len(dst.Mounts) != 1 || dst.Mounts[0].Host != "/h" {
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
