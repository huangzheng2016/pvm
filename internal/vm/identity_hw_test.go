package vm

import (
	"regexp"
	"testing"

	"github.com/huangzheng2016/pvm/internal/qemu"
)

func TestHardwareIdentityStableAndRefreshedOnClone(t *testing.T) {
	src := &Spec{Name: "dev1", User: "dev", SmbiosSpoof: true}
	if err := src.EnsureHardwareIdentity(); err != nil {
		t.Fatal(err)
	}
	uuidRE := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	serialRE := regexp.MustCompile(`^[A-Z0-9]{12}$`)
	if !uuidRE.MatchString(src.UUID) || src.SMBIOSSerials == nil {
		t.Fatalf("invalid identity: %+v", src)
	}
	for _, s := range []string{src.SMBIOSSerials.System, src.SMBIOSSerials.Board, src.SMBIOSSerials.Chassis} {
		if !serialRE.MatchString(s) {
			t.Fatalf("invalid serial %q", s)
		}
	}
	origUUID, origSerial := src.UUID, src.SMBIOSSerials.System
	if err := src.EnsureHardwareIdentity(); err != nil || src.UUID != origUUID || src.SMBIOSSerials.System != origSerial {
		t.Fatal("existing hardware identity must not rotate")
	}

	clone := CloneSpec(src, "dev2", "100.99.1.11", "10:65:30:01:01:0b")
	if clone.UUID == src.UUID || clone.SMBIOSSerials.System == src.SMBIOSSerials.System || clone.SMBIOSSerials.Board == src.SMBIOSSerials.Board {
		t.Fatal("clone inherited source hardware identity")
	}
}

func TestQEMUArgsIncludeUUIDAndPerVMSMBIOSSerials(t *testing.T) {
	c := &qemu.VMConfig{
		Name: "dev1", CPUs: 2, MemMB: 1024, DiskPath: "/d/d.qcow2", SeedPath: "/d/seed.iso", VarsPath: "/d/vars.fd",
		QMPSock: "tcp:127.0.0.1:44001", SerialSock: "tcp:127.0.0.1:44002",
		ConsoleLog: "/d/console.log", Accel: qemu.AccelTCG,
		SmbiosSpoof: true, UUID: "11111111-2222-4333-8444-555555555555",
		SMBIOSSerials: [3]string{"SYS123456789", "BRD123456789", "CHS123456789"},
	}
	tc := &qemu.Toolchain{FirmwareDir: "/fw"}
	args, err := qemu.BuildArgs(tc, c)
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, a := range args {
		joined += a + "\n"
	}
	for _, want := range []string{
		"-uuid\n11111111-2222-4333-8444-555555555555",
		"serial=SYS123456789", "serial=BRD123456789", "serial=CHS123456789",
		"uuid=11111111-2222-4333-8444-555555555555",
	} {
		if !regexp.MustCompile(regexp.QuoteMeta(want)).MatchString(joined) {
			t.Errorf("missing %q in args:\n%s", want, joined)
		}
	}
}
