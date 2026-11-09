package qemu

import (
	"strings"
	"testing"
)

func TestBuildArgsWithoutVirtio9p(t *testing.T) {
	unsupported := false
	tc := &Toolchain{System: "qemu-test", FirmwareDir: "/fw", has9p: &unsupported}
	cfg := testCfg(AccelTCG)
	if _, err := BuildArgs(tc, cfg); err == nil || !strings.Contains(err.Error(), "--share none") {
		t.Fatalf("default share must fail explicitly without 9p: %v", err)
	}
	cfg.NoShare = true
	cfg.ExtraMounts = nil
	if _, err := BuildArgs(tc, cfg); err != nil {
		t.Fatalf("no-share VM should build without 9p: %v", err)
	}
	cfg.ExtraMounts = []Mount{{Tag: "pvm0", HostPath: "/d/mounts/slot1"}}
	if _, err := BuildArgs(tc, cfg); err == nil {
		t.Fatal("extra mount must fail without 9p")
	}
}
