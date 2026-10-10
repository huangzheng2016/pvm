package qemu

import (
	"strings"
	"testing"
)

func testCfg(arch string, accel Accel) *VMConfig {
	return &VMConfig{
		Name: "t1", Arch: arch, CPUs: 2, MemMB: 2048,
		DiskPath: "/d/disk.qcow2", SeedPath: "/d/seed.iso", VarsPath: "/d/vars.fd",
		ConsoleLog: "/d/console.log", QMPSock: "/d/qmp.sock", SerialSock: "/d/serial.sock",
		SSHHostPort: 10022, ShareDir: "/share",
		ExtraMounts: []Mount{{Tag: "pvm0", HostPath: "/host/dir"}},
		Accel:       accel,
	}
}

func joined(args []string) string { return strings.Join(args, " ") }

func TestBuildArgsARM64HVF(t *testing.T) {
	tc := &Toolchain{FirmwareDir: "/fw"}
	args, err := BuildArgs(tc, testCfg("arm64", AccelHVF))
	if err != nil {
		t.Fatal(err)
	}
	j := joined(args)
	for _, want := range []string{
		"-machine virt", "-accel hvf", "-cpu host",
		"if=pflash,format=raw,readonly=on,file=/fw/edk2-aarch64-code.fd",
		"if=pflash,format=raw,file=/d/vars.fd",
		"hostfwd=tcp:127.0.0.1:10022-:22",
		"mount_tag=pvmshare", "mount_tag=pvm0",
		"security_model=mapped-xattr",
		"-qmp unix:/d/qmp.sock,server=on,wait=off",
	} {
		if !strings.Contains(j, want) {
			t.Errorf("args missing %q\nargs: %s", want, j)
		}
	}
}

func TestBuildArgsAMD64NoPflash(t *testing.T) {
	tc := &Toolchain{}
	args, err := BuildArgs(tc, testCfg("amd64", AccelTCG))
	if err != nil {
		t.Fatal(err)
	}
	j := joined(args)
	if strings.Contains(j, "pflash") {
		t.Errorf("amd64 SeaBIOS boot must not use pflash: %s", j)
	}
	for _, want := range []string{"-machine q35", "-accel tcg,thread=multi", "-cpu max"} {
		if !strings.Contains(j, want) {
			t.Errorf("args missing %q", want)
		}
	}
}

func TestBuildArgsARM64NeedsFirmware(t *testing.T) {
	_, err := BuildArgs(&Toolchain{}, testCfg("arm64", AccelHVF))
	if err == nil {
		t.Fatal("expected error when firmware dir unknown")
	}
}

func TestAccelArgsAndFallback(t *testing.T) {
	if got := AccelTCG.Args(); !strings.Contains(joined(got), "thread=multi") {
		t.Errorf("tcg args: %v", got)
	}
	if fb, ok := AccelHVF.Fallback(); !ok || fb != AccelTCG {
		t.Errorf("hvf fallback = %v,%v", fb, ok)
	}
	if _, ok := AccelTCG.Fallback(); ok {
		t.Error("tcg has no further fallback")
	}
}

func TestParseAccel(t *testing.T) {
	if _, ok := ParseAccel("kvm"); !ok {
		t.Error("kvm should parse")
	}
	if _, ok := ParseAccel("nope"); ok {
		t.Error("nope should not parse")
	}
}

func TestQemuArch(t *testing.T) {
	if QemuArch("amd64") != "x86_64" || QemuArch("arm64") != "aarch64" {
		t.Error("arch mapping wrong")
	}
}
