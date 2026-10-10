package vm

import (
	"strings"
	"testing"
)

func TestUserDataGoldenSkipsApt(t *testing.T) {
	base := &Spec{Name: "dev1", User: "u", GuestUID: 501}
	ud, err := userData(base, "ssh-ed25519 AAAA", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ud), "package_update: true") {
		t.Error("base image should run package_update")
	}
	if !strings.Contains(string(ud), "qemu-guest-agent") {
		t.Error("base image should install qemu-guest-agent")
	}

	golden := &Spec{Name: "dev1", User: "u", GuestUID: 501, Golden: 2}
	ud, err = userData(golden, "ssh-ed25519 AAAA", nil)
	if err != nil {
		t.Fatal(err)
	}
	s := string(ud)
	if strings.Contains(s, "package_update: true") {
		t.Error("golden clone must skip package_update")
	}
	if strings.Contains(s, "packages:") {
		t.Error("golden clone must not install packages")
	}
	if !strings.Contains(s, "docker") {
		t.Error("golden clone user should join the docker group")
	}
}
