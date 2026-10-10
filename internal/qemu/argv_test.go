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
		Network:     Network{Mode: NetSlirp, SSHHostPort: 10022},
		ShareDir:    "/share",
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
		"security_model=none",
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

func TestNetArgsSlirpPool(t *testing.T) {
	args := joined(netArgs(Network{Mode: NetSlirp, SSHHostPort: 10022, PoolBase: 30000, PoolSize: 2}))
	for _, want := range []string{
		"hostfwd=tcp:127.0.0.1:10022-:22",
		"hostfwd=tcp:127.0.0.1:30000-:30000",
		"hostfwd=udp:127.0.0.1:30000-:30000",
		"hostfwd=tcp:127.0.0.1:30001-:30001",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("slirp pool args missing %q", want)
		}
	}
	if strings.Contains(args, "30002") {
		t.Errorf("pool overflow: %s", args)
	}
}

func TestNetArgsCluster(t *testing.T) {
	args := joined(netArgs(Network{Mode: NetCluster, LinkSock: "/l/dev1.sock", MAC: "52:54:00:01:00:0a"}))
	for _, want := range []string{
		"pcie-root-port,id=rp-link",
		"-netdev stream,id=net0,server=off,addr.type=unix,addr.path=/l/dev1.sock",
		"virtio-net-pci,id=nic0,netdev=net0,mac=52:54:00:01:00:0a,bus=rp-link",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("cluster args missing %q\ngot: %s", want, args)
		}
	}
}

func TestBuildArgsSmbiosSpoof(t *testing.T) {
	c := testCfg("arm64", AccelHVF)
	c.SmbiosSpoof = true
	args, err := BuildArgs(&Toolchain{FirmwareDir: "/fw"}, c)
	if err != nil {
		t.Fatal(err)
	}
	j := joined(args)
	if !strings.Contains(j, "-smbios type=1,manufacturer=Dell Inc.,product=Inspiron 15 5510") {
		t.Errorf("spoof args missing: %s", j)
	}
	args, err = BuildArgs(&Toolchain{FirmwareDir: "/fw"}, testCfg("arm64", AccelHVF))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(joined(args), "-smbios") {
		t.Error("smbios must stay at QEMU defaults unless spoofing is requested")
	}
}
