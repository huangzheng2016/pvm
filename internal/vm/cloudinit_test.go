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

func TestUserDataRegionProfile(t *testing.T) {
	plain := &Spec{Name: "dev1", User: "u", GuestUID: 501}
	ud, err := userData(plain, "ssh-ed25519 AAAA", nil)
	if err != nil {
		t.Fatal(err)
	}
	s := string(ud)
	if !strings.Contains(s, "locale: en_US.UTF-8") {
		t.Error("default locale should be en_US.UTF-8")
	}
	if strings.Contains(s, "timezone:") {
		t.Error("no timezone key unless requested")
	}
	if strings.Contains(s, "git config") {
		t.Error("no git identity unless requested")
	}

	s2 := &Spec{Name: "dev1", User: "u", GuestUID: 501, TZ: "America/Los_Angeles", Locale: "en_US.UTF-8", GitEmail: "dev@example.com", GitName: "Dev"}
	ud, err = userData(s2, "ssh-ed25519 AAAA", nil)
	if err != nil {
		t.Fatal(err)
	}
	s = string(ud)
	for _, want := range []string{"timezone: America/Los_Angeles", `user.email "dev@example.com"`, `user.name "Dev"`} {
		if !strings.Contains(s, want) {
			t.Errorf("user-data missing %q", want)
		}
	}
}

// The seed must never leak the host's git identity into the guest
// (Claude Code uploads gitStatus session context).
func TestUserDataNeverContainsHostGitconfig(t *testing.T) {
	host := map[string]string{"user.name": "host-real-name", "user.email": "host-real@example.com"}
	s := &Spec{Name: "dev1", User: "u", GuestUID: 501, GitEmail: "neutral@example.com"}
	ud, err := userData(s, "ssh-ed25519 AAAA", nil)
	if err != nil {
		t.Fatal(err)
	}
	out := string(ud)
	for _, leak := range host {
		if strings.Contains(out, leak) {
			t.Errorf("seed leaks host git identity %q", leak)
		}
	}
	if !strings.Contains(out, "neutral@example.com") {
		t.Error("explicitly requested git email should be written")
	}
}
