package image

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/huangzheng2016/pvm/internal/qemu"
)

// The base image boots without noxsave, so under WHPX the first boot must
// use qemu64; the cold boot after cloud-init uses the full config.
func TestFirstBootConfigWithoutGuestKernelArgs(t *testing.T) {
	cfg := qemu.VMConfig{Accel: qemu.AccelWHPX, GuestKernelArgs: qemu.AccelWHPX.GuestKernelArgs()}
	first := firstBootConfig(cfg)
	if first.GuestKernelArgs != "" {
		t.Fatalf("first boot keeps kernel args %q", first.GuestKernelArgs)
	}
	if args := first.Accel.ArgsForGuest(first.GuestKernelArgs); !slices.Contains(args, "qemu64") {
		t.Errorf("first WHPX boot cpu args = %v, want qemu64", args)
	}
	if args := cfg.Accel.ArgsForGuest(cfg.GuestKernelArgs); !slices.Contains(args, "host,-xsave") {
		t.Errorf("cold boot cpu args = %v, want host,-xsave", args)
	}
	if cfg.GuestKernelArgs == "" {
		t.Error("firstBootConfig modified its input")
	}
}

func TestGuestPanic(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "console.log")
	if got := guestPanic(log); got != "" {
		t.Errorf("missing log: %q", got)
	}
	os.WriteFile(log, []byte("[ 1.0] booting\r\n[ 2.0] cloud-init: ok\r\n"), 0o600)
	if got := guestPanic(log); got != "" {
		t.Errorf("clean log: %q", got)
	}
	os.WriteFile(log, []byte("[ 70.5] key\r\nP+q6E616D65\\[ 1545.18] Kernel panic - not syncing: Attempted to kill init! exitcode=0x0000008b\r\n[ 1545.19] CPU: 2\r\n"), 0o600)
	want := "Kernel panic - not syncing: Attempted to kill init! exitcode=0x0000008b"
	if got := guestPanic(log); got != want {
		t.Errorf("guestPanic = %q, want %q", got, want)
	}
	if note := guestPanicNote(log); !strings.Contains(note, want) || !strings.Contains(note, "--keep-vm") {
		t.Errorf("guestPanicNote = %q", note)
	}
}

func TestSSHRunnerKeepalive(t *testing.T) {
	args := strings.Join((&sshRunner{key: "k", port: 10022, user: "baker"}).args("true"), " ")
	if !strings.Contains(args, "ServerAliveInterval=") || !strings.Contains(args, "ServerAliveCountMax=") {
		t.Errorf("bake ssh has no keepalive: %s", args)
	}
}
