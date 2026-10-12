package qemu

import (
	"errors"
	"strings"
	"testing"

	"github.com/huangzheng2016/pvm/internal/native"
)

func testCfg(accel Accel) *VMConfig {
	return &VMConfig{
		Name: "t1", CPUs: 2, MemMB: 2048,
		DiskPath: "/d/disk.qcow2", SeedPath: "/d/seed.iso", VarsPath: "/d/vars.fd",
		ConsoleLog: "/d/console.log", QMPSock: "/d/qmp.sock", SerialSock: "/d/serial.sock",
		Network:     Network{Mode: NetSlirp, SSHHostPort: 10022},
		ShareDir:    "/share",
		ExtraMounts: []Mount{{Tag: "pvm0", HostPath: "/host/dir"}},
		Accel:       accel,
	}
}

func joined(args []string) string { return strings.Join(args, " ") }

func TestBuildArgsNativeHVF(t *testing.T) {
	tc := &Toolchain{FirmwareDir: "/fw"}
	args, err := BuildArgs(tc, testCfg(AccelHVF))
	if err != nil {
		t.Fatal(err)
	}
	j := joined(args)
	want := []string{
		"-accel hvf", "-cpu host",
		"hostfwd=tcp:127.0.0.1:10022-:22",
		"mount_tag=pvmshare", "mount_tag=pvm0",
		"security_model=none",
		"-qmp unix:/d/qmp.sock,server=on,wait=off",
	}
	if native.Arch == "arm64" {
		want = append(want, "-machine virt", "if=pflash,format=raw,readonly=on,file=/fw/edk2-aarch64-code.fd", "if=pflash,format=raw,file=/d/vars.fd")
	} else {
		want = append(want, "-machine q35")
	}
	for _, w := range want {
		if !strings.Contains(j, w) {
			t.Errorf("args missing %q\nargs: %s", w, j)
		}
	}
}

func TestBuildArgsNativeTCG(t *testing.T) {
	tc := &Toolchain{FirmwareDir: "/fw"}
	args, err := BuildArgs(tc, testCfg(AccelTCG))
	if err != nil {
		t.Fatal(err)
	}
	j := joined(args)
	if native.Arch == "amd64" {
		if strings.Contains(j, "pflash") {
			t.Errorf("amd64 SeaBIOS boot must not use pflash: %s", j)
		}
		if !strings.Contains(j, "-machine q35") {
			t.Errorf("amd64 args missing q35: %s", j)
		}
	} else if !strings.Contains(j, "-machine virt") {
		t.Errorf("arm64 args missing virt: %s", j)
	}
	for _, want := range []string{"-accel tcg,thread=multi", "-cpu max"} {
		if !strings.Contains(j, want) {
			t.Errorf("args missing %q", want)
		}
	}
}

func TestBuildArgsNativeNeedsFirmware(t *testing.T) {
	if native.Arch != "arm64" {
		t.Skip("UEFI firmware check is arm64-specific")
	}
	_, err := BuildArgs(&Toolchain{}, testCfg(AccelHVF))
	if err == nil {
		t.Fatal("expected error when firmware dir unknown")
	}
}

func TestBuildArgsGPU(t *testing.T) {
	cfg := testCfg(AccelHVF)
	args, err := BuildArgs(&Toolchain{FirmwareDir: "/fw"}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(joined(args), "virtio-gpu") {
		t.Error("gpu off must not add virtio-gpu")
	}
	cfg.GPU = true
	args, err = BuildArgs(&Toolchain{FirmwareDir: "/fw"}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(joined(args), "-device virtio-gpu-pci") {
		t.Error("gpu on must add virtio-gpu-pci")
	}
	if strings.Contains(joined(args), "virtio-balloon") {
		t.Error("balloon off must not add virtio-balloon")
	}
	cfg.Balloon = true
	args, err = BuildArgs(&Toolchain{FirmwareDir: "/fw"}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(joined(args), "virtio-balloon-pci,free-page-reporting=on") {
		t.Error("balloon on must add virtio-balloon with free-page-reporting")
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
	if h := FallbackHint(errors.New("ret = HV_NO_DEVICE (0xfae94006)")); !strings.Contains(h, "reboot the host") {
		t.Errorf("HV_NO_DEVICE hint: %q", h)
	}
	if h := FallbackHint(errors.New("some other error")); h != "" {
		t.Errorf("unrelated error must have no hint: %q", h)
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
	want := "x86_64"
	if native.Arch == "arm64" {
		want = "aarch64"
	}
	if got := QemuArch(); got != want {
		t.Fatalf("QemuArch() = %q, want %q", got, want)
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
	c := testCfg(AccelHVF)
	c.SmbiosSpoof = true
	args, err := BuildArgs(&Toolchain{FirmwareDir: "/fw"}, c)
	if err != nil {
		t.Fatal(err)
	}
	j := joined(args)
	for _, want := range []string{"product=OptiPlex 7010", "type=0,vendor=Dell Inc.", "type=3,manufacturer=Dell Inc."} {
		if !strings.Contains(j, want) {
			t.Errorf("spoof args missing %q: %s", want, j)
		}
	}
	args, err = BuildArgs(&Toolchain{FirmwareDir: "/fw"}, testCfg(AccelHVF))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(joined(args), "-smbios") {
		t.Error("smbios must stay at QEMU defaults unless spoofing is requested")
	}
}
