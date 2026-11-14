package sftpd

import (
	"os/exec"
	"strings"
	"testing"
)

func TestGuestCommandsValidatePath(t *testing.T) {
	for _, p := range []string{"", "/workspace", "/workspace/", "/workspace/../etc", "/workspace/Bad", "/workspace/a b", "/workspace/a;rm", "/tmp/x", "/workspace/a/b"} {
		if _, err := MountCommand(p, false); err == nil {
			t.Errorf("MountCommand(%q) accepted", p)
		}
		if _, err := UnmountCommand(p); err == nil {
			t.Errorf("UnmountCommand(%q) accepted", p)
		}
	}
	rw, err := MountCommand("/workspace/dev1", false)
	if err != nil {
		t.Fatal(err)
	}
	ro, err := MountCommand("/workspace/dev1", true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rw, "exec sshfs -f -o passive,") || !strings.HasSuffix(rw, " :/ /workspace/dev1") || strings.Contains(rw, ",ro") || !strings.Contains(rw, ",uid=$(id -u),gid=$(id -g)") {
		t.Errorf("read-write mount command = %q", rw)
	}
	if !strings.Contains(ro, ",ro ") {
		t.Errorf("read-only mount command = %q", ro)
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh to check syntax")
	}
	mounted, _ := MountedCommand("/workspace/dev1")
	unmount, _ := UnmountCommand("/workspace/dev1")
	for _, cmd := range []string{InstallCommand(), rw, ro, mounted, unmount} {
		if out, err := exec.Command(sh, "-n", "-c", cmd).CombinedOutput(); err != nil {
			t.Errorf("sh -n %q: %v: %s", cmd, err, out)
		}
	}
}

// Ubuntu's fusermount3 AppArmor profile rejects mounts at /workspace.
func TestInstallCommandAllowsWorkspaceMounts(t *testing.T) {
	cmd := InstallCommand()
	for _, want := range []string{
		"/etc/apparmor.d/local/fusermount3",
		"-> /workspace/*/,",
		"umount /workspace/*/,",
		"apparmor_parser -r /etc/apparmor.d/fusermount3",
		"printf '%s\\n'",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("install command lacks %q:\n%s", want, cmd)
		}
	}
}
