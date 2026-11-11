package vm

import (
	"runtime"
	"strings"
	"testing"
)

func TestUserDataSkipsApt(t *testing.T) {
	base := &Spec{Name: "dev1", User: "u", GuestUID: 501}
	ud, err := userData(base, "ssh-ed25519 AAAA", nil)
	if err != nil {
		t.Fatal(err)
	}
	s := string(ud)
	if !strings.Contains(s, "package_update: false") || strings.Contains(s, "packages:") {
		t.Errorf("base image must not install an unused guest agent before pvm's agent upload:\n%s", s)
	}

	golden := &Spec{Name: "dev1", User: "u", GuestUID: 501, Golden: 2}
	ud, err = userData(golden, "ssh-ed25519 AAAA", nil)
	if err != nil {
		t.Fatal(err)
	}
	s = string(ud)
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

func TestGatewayUserDataDoesNotNeedPackageDownloads(t *testing.T) {
	ud, err := gatewayUserData(&Spec{Name: "pvm-gw1", User: "pvmgw", GuestUID: 1000, ClusterID: 1}, "ssh-ed25519 AAAA")
	if err != nil {
		t.Fatal(err)
	}
	out := string(ud)
	if !strings.Contains(out, "package_update: false") || strings.Contains(out, "packages:") {
		t.Fatalf("gateway must not wait for apt before DNS is ready:\n%s", out)
	}
	if runtime.GOOS == "windows" && !strings.Contains(out, "DNS=100.99.1.1") {
		t.Fatalf("native Windows gateway must use its TCP-capable DNS relay:\n%s", out)
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
	for _, want := range []string{"timezone: America/Los_Angeles", "/home/u/.gitconfig", "email = dev@example.com", "name = Dev"} {
		if !strings.Contains(s, want) {
			t.Errorf("user-data missing %q", want)
		}
	}
	if strings.Contains(s, "git config --global") {
		t.Error("git identity must not be written via shell")
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

func TestGuestMountOptions(t *testing.T) {
	tests := []struct {
		profile string
		want    string
	}{
		{"", "trans=virtio,version=9p2000.L,access=any,cache=none,msize=262144"},
		{MountProfileCoherent, "trans=virtio,version=9p2000.L,access=any,cache=none,msize=262144"},
		{MountProfileReadahead, "trans=virtio,version=9p2000.L,access=any,cache=readahead,msize=1048576"},
	}
	for _, tc := range tests {
		got, err := GuestMountOptions(tc.profile)
		if err != nil {
			t.Errorf("GuestMountOptions(%q): %v", tc.profile, err)
		} else if got != tc.want {
			t.Errorf("GuestMountOptions(%q) = %q, want %q", tc.profile, got, tc.want)
		}
	}
	for _, profile := range []string{"loose", "fscache", "cache=none,msize=4096", "readahead;id"} {
		if _, err := GuestMountOptions(profile); err == nil {
			t.Errorf("GuestMountOptions(%q) unexpectedly passed", profile)
		}
	}
}

func TestUserDataUsesMountProfiles(t *testing.T) {
	spec := &Spec{Name: "dev1", User: "u", GuestUID: 501}
	mounts := []GuestMount{
		{Tag: "pvm0", Host: "/h", Guest: "/work"},
		{Tag: "pvm1", Host: "/h", Guest: "/data", Profile: MountProfileReadahead},
	}
	ud, err := userData(spec, "ssh-ed25519 AAAA", mounts)
	if err != nil {
		t.Fatal(err)
	}
	coherent, err := GuestMountOptions("")
	if err != nil {
		t.Fatal(err)
	}
	readahead, err := GuestMountOptions(MountProfileReadahead)
	if err != nil {
		t.Fatal(err)
	}
	out := string(ud)
	if !strings.Contains(out, "pvmshare /share 9p "+coherent+",rw,_netdev,nofail") {
		t.Errorf("default /share is not coherent: %s", out)
	}
	if got := strings.Count(out, coherent); got != 4 {
		t.Errorf("coherent options occur %d times, want /share and legacy mount fstab+mount", got)
	}
	if got := strings.Count(out, readahead); got != 2 {
		t.Errorf("readahead options occur %d times, want fstab+mount", got)
	}
}

func TestUserDataWithoutSharesOmitsEmptyRuncmd(t *testing.T) {
	ud, err := userData(&Spec{Name: "dev1", User: "dev", GuestUID: 1000, ShareMode: ShareNone}, "ssh-ed25519 AAAA", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ud), "runcmd: []") {
		t.Fatalf("empty runcmd is rejected by cloud-init schema:\n%s", ud)
	}
	ud, err = userData(&Spec{Name: "dev1", User: "dev", GuestUID: 1000, ShareMode: ShareNone, Golden: 1}, "ssh-ed25519 AAAA", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ud), "systemctl enable --now docker") {
		t.Fatalf("golden initialization was removed with empty shares:\n%s", ud)
	}
}

func TestUserDataKernelArgsGrubDropIn(t *testing.T) {
	const dropIn = "/etc/default/grub.d/99-pvm-kernel-args.cfg"
	plain := &Spec{Name: "dev1", User: "u", GuestUID: 501}
	ud, err := userData(plain, "ssh-ed25519 AAAA", nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ud), dropIn) || strings.Contains(string(ud), "update-grub") {
		t.Error("no kernel args must leave grub alone")
	}

	for _, spec := range []*Spec{
		{Name: "dev1", User: "u", GuestUID: 501, KernelArgs: "noxsave"},
		{Name: "dev1", User: "u", GuestUID: 501, KernelArgs: "noxsave", ShareMode: ShareNone},
		{Name: "pvm-gw1", User: "pvmgw", GuestUID: 501, KernelArgs: "noxsave", Role: "gateway"},
	} {
		gen := userData
		if spec.Role == "gateway" {
			gen = func(s *Spec, pub string, _ []GuestMount) ([]byte, error) { return gatewayUserData(s, pub) }
		}
		ud, err := gen(spec, "ssh-ed25519 AAAA", nil)
		if err != nil {
			t.Fatal(err)
		}
		s := string(ud)
		if !strings.Contains(s, dropIn) || !strings.Contains(s, `$GRUB_CMDLINE_LINUX_DEFAULT noxsave`) {
			t.Errorf("%s: missing grub drop-in:\n%s", spec.Name, s)
		}
		if !strings.Contains(s, "- update-grub") {
			t.Errorf("%s: missing update-grub:\n%s", spec.Name, s)
		}
	}

	for _, bad := range []string{"noxsave; reboot", "a\nb", "init=/bin/sh $(x)", "NOXSAVE"} {
		spec := &Spec{Name: "dev1", User: "u", GuestUID: 501, KernelArgs: bad}
		if _, err := userData(spec, "ssh-ed25519 AAAA", nil); err == nil {
			t.Errorf("kernel args %q accepted", bad)
		}
		if _, err := gatewayUserData(spec, "ssh-ed25519 AAAA"); err == nil {
			t.Errorf("gateway kernel args %q accepted", bad)
		}
	}
}
