package vm

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	diskfs "github.com/diskfs/go-diskfs"
)

func TestSeedImage(t *testing.T) {
	dir := t.TempDir()
	seed := filepath.Join(dir, "seed.img")
	ud := []byte("#cloud-config\nhostname: t1\n")
	md := []byte("instance-id: pvm-t1\nlocal-hostname: t1\n")
	if err := WriteSeed(seed, ud, md); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(seed)
	if err != nil || fi.Size() == 0 {
		t.Fatalf("seed image missing or empty: %v", err)
	}

	d, err := diskfs.Open(seed)
	if err != nil {
		t.Fatal(err)
	}
	fs, err := d.GetFilesystem(0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := fs.ReadFile("/user-data")
	if err != nil {
		t.Fatalf("read user-data back: %v", err)
	}
	if string(got) != string(ud) {
		t.Errorf("user-data round trip mismatch: %q", got)
	}
	if _, err := fs.ReadFile("/meta-data"); err != nil {
		t.Errorf("read meta-data back: %v", err)
	}
}

func TestUserDataContainsKeyAndShare(t *testing.T) {
	spec := &Spec{Name: "dev1", User: "alice"}
	ud, err := userData(spec, "ssh-ed25519 AAAA test", []GuestMount{{Tag: "pvm0", Host: "/h", Guest: "/work"}})
	if err != nil {
		t.Fatal(err)
	}
	s := string(ud)
	for _, want := range []string{
		"ssh-ed25519 AAAA test", "alice", "pvmshare /share 9p",
		"pvm0 /work 9p", "lock_passwd: true", "ssh_pwauth: false",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("user-data missing %q", want)
		}
	}
}

func TestAllocSSHPortSkipsUsed(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	occupied := l.Addr().(*net.TCPAddr).Port

	base := occupied - 1
	if base < 1024 {
		base = 1024
	}
	got, err := AllocSSHPort(map[int]bool{base: true}, base)
	if err != nil {
		t.Fatal(err)
	}
	if got == base {
		t.Errorf("returned used port %d", got)
	}
	if got == occupied {
		t.Errorf("returned host-bound port %d", got)
	}
}
