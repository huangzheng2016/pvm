package vm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/qemu"
)

func TestCreatePrivateSharesAndDistinctManagementKeys(t *testing.T) {
	p := paths.New(t.TempDir())
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(p.Images, "base.qcow2")
	if err := os.WriteFile(base, nil, 0600); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(t.TempDir(), "qemu-img")
	if err := os.WriteFile(img, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	var pubs []string
	for _, name := range []string{"dev1", "dev2"} {
		spec := &Spec{Name: name, User: "dev", BaseDisk: base}
		if err := Create(p, &qemu.Toolchain{Img: img}, spec); err != nil {
			t.Fatal(err)
		}
		if spec.ShareMode != SharePrivate || spec.SSHKeyName != name {
			t.Fatalf("privacy defaults missing: %+v", spec)
		}
		if _, err := os.Stat(p.VMShare(name)); err != nil {
			t.Fatal(err)
		}
		if spec.SharedDir(p) == p.Share {
			t.Fatal("new VM exposes global share")
		}
		pub, err := os.ReadFile(spec.SSHKeyPath(p) + ".pub")
		if err != nil {
			t.Fatal(err)
		}
		pubs = append(pubs, string(pub))
		loaded, err := Load(p, name)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.SSHKeyPath(p) != spec.SSHKeyPath(p) || loaded.ShareMode != SharePrivate {
			t.Fatal("privacy configuration did not persist")
		}
	}
	if pubs[0] == pubs[1] {
		t.Fatal("management public keys identify both VMs")
	}
}

func TestExistingVMKeepsItsShareAndKey(t *testing.T) {
	p := paths.New(t.TempDir())
	if err := os.MkdirAll(p.VMDir("dev1"), 0700); err != nil {
		t.Fatal(err)
	}
	spec := &Spec{Name: "dev1", User: "dev"}
	if err := spec.Save(p); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p, "dev1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ShareMode != ShareGlobal || got.SharedDir(p) != p.Share || got.SSHKeyPath(p) != p.SSHKey() {
		t.Fatal("existing VM identity or share silently changed")
	}
}

func TestNoShareOmitsGuestAndQEMUExposure(t *testing.T) {
	p := paths.New(t.TempDir())
	spec := &Spec{Name: "dev1", User: "dev", ShareMode: ShareNone}
	data, err := userData(spec, "ssh-ed25519 AAAA", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "pvmshare") || strings.Contains(string(data), "/share") {
		t.Fatal("no-share guest still mounts a share")
	}
	cfg := spec.QEMUConfig(p, qemu.AccelTCG, "", 0, 0)
	if !cfg.NoShare || cfg.ShareDir != "" {
		t.Fatal("no-share VM still exports a host share")
	}
	spec.Role = "gateway"
	spec.ShareMode = ShareGlobal
	cfg = spec.QEMUConfig(p, qemu.AccelTCG, "", 0, 0)
	if !cfg.NoShare || cfg.MountDir != "" {
		t.Fatal("gateway exposes host shares")
	}
}

func TestReadOnlyMountAtBothBoundaries(t *testing.T) {
	p := paths.New(t.TempDir())
	spec := &Spec{Name: "dev1", User: "dev", Mounts: []GuestMount{{Tag: "pvm0", Guest: "/work", ReadOnly: true}}}
	data, err := userData(spec, "ssh-ed25519 AAAA", spec.Mounts)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), ",ro,_netdev") {
		t.Fatal("guest fstab does not enforce readonly")
	}
	cfg := spec.QEMUConfig(p, qemu.AccelTCG, "", 0, 0)
	if !cfg.ExtraMounts[0].ReadOnly {
		t.Fatal("readonly lost at QEMU boundary")
	}
}

func TestRejectInvalidShareAndKeyNames(t *testing.T) {
	if err := ValidateSpec(&Spec{User: "dev", ShareMode: "invalid"}, nil); err == nil {
		t.Fatal("invalid share mode accepted")
	}
	if err := ValidateSpec(&Spec{User: "dev", SSHKeyName: "../dev1"}, nil); err == nil {
		t.Fatal("invalid key name accepted")
	}
}
