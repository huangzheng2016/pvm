package qemu

import (
	"strings"
	"testing"
)

func TestNoShareHasNoDefaultFilesystemBackend(t *testing.T) {
	cfg := testCfg(AccelTCG)
	cfg.NoShare = true
	cfg.ExtraMounts[0].ReadOnly = true
	args, err := BuildArgs(&Toolchain{FirmwareDir: "/fw"}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	text := joined(args)
	if strings.Contains(text, "mount_tag=pvmshare") || strings.Contains(text, "local,id=fs0,") {
		t.Fatal("disabled share still has a host backend")
	}
	if !strings.Contains(text, "local,id=fs1,path=/d/mounts/slot1,security_model=none,readonly=on") {
		t.Fatal("readonly must be enforced by QEMU, not just guest mount options")
	}
	cfg.MountDir = ""
	cfg.ExtraMounts = nil
	args, err = BuildArgs(&Toolchain{FirmwareDir: "/fw"}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(joined(args), "-fsdev") {
		t.Fatal("standalone no-share VM still has filesystem backends")
	}
}

func TestNoShareRestoreDoesNotAttachDefaultDevice(t *testing.T) {
	in := &Instance{Cfg: VMConfig{NoShare: true}}
	if err := in.reattachShares(nil); err != nil {
		t.Fatal(err)
	}
}
