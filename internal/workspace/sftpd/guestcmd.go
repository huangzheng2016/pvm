package sftpd

import (
	"fmt"
	"strings"

	"github.com/huangzheng2016/pvm/internal/workspace"
)

// apparmorRule extends Ubuntu's fusermount3 profile, which only allows
// FUSE mounts under the home directory, /mnt, /media, /tmp and /run/user.
// The profile includes local/fusermount3 for site additions.
var apparmorRule = []string{
	"# pvm workspaces: sshfs mounts at /workspace/<id>",
	"mount fstype=fuse.sshfs options=(nosuid,nodev) options in (ro,rw) -> /workspace/*/,",
	"umount /workspace/*/,",
}

// InstallCommand installs sshfs in the guest unless it is already there,
// and lets fusermount3 mount at workspace paths where AppArmor confines it.
func InstallCommand() string {
	const apt = "sudo -n env DEBIAN_FRONTEND=noninteractive apt-get -q -o DPkg::Lock::Timeout=300"
	const profile, local = "/etc/apparmor.d/fusermount3", "/etc/apparmor.d/local/fusermount3"
	return "{ command -v sshfs >/dev/null 2>&1 || { " + apt + " update >&2 && " + apt + " install -y --no-install-recommends sshfs >&2; }; } && " +
		"{ [ ! -f " + profile + " ] || sudo -n grep -qsxF '" + apparmorRule[0] + "' " + local + " || " +
		"{ printf '%s\\n' '" + strings.Join(apparmorRule, "' '") + "' | sudo -n tee -a " + local + " >/dev/null && " +
		"{ [ ! -d /sys/kernel/security/apparmor ] || sudo -n apparmor_parser -r " + profile + "; }; }; } >&2"
}

func checkGuestPath(guestPath string) error {
	id, ok := strings.CutPrefix(guestPath, workspace.GuestRoot+"/")
	if !ok || workspace.ValidID(id) != nil || guestPath != workspace.GuestPathFor(id) {
		return fmt.Errorf("invalid workspace guest path %q", guestPath)
	}
	return nil
}

// unmount lazily detaches a mount left by a dead session, so a stale
// endpoint cannot hang the next mount or the caller.
func unmount(p string) string {
	return "fusermount3 -u -z " + p + " >/dev/null 2>&1 || fusermount -u -z " + p + " >/dev/null 2>&1"
}

// MountCommand runs sshfs in passive mode: the SFTP stream is the ssh
// session's stdin and stdout, so nothing else may write to stdout.
// Caches are off so host edits show up promptly. The host server reports
// uid 0, which idmap=user would leave as root, so uid and gid force the
// login user; git refuses repositories owned by someone else.
func MountCommand(guestPath string, readOnly bool) (string, error) {
	if err := checkGuestPath(guestPath); err != nil {
		return "", err
	}
	opts := "passive,dir_cache=no,entry_timeout=0,attr_timeout=0,negative_timeout=0,uid=$(id -u),gid=$(id -g)"
	if readOnly {
		opts += ",ro"
	}
	return "{ " + unmount(guestPath) + "; " +
		"[ -d " + guestPath + " ] || { sudo -n mkdir -p " + guestPath + " && sudo -n chown \"$(id -u):$(id -g)\" " + workspace.GuestRoot + " " + guestPath + "; }; } >&2 && " +
		"exec sshfs -f -o " + opts + " :/ " + guestPath, nil
}

// MountedCommand exits 0 only while an sshfs mount is at guestPath.
func MountedCommand(guestPath string) (string, error) {
	if err := checkGuestPath(guestPath); err != nil {
		return "", err
	}
	return "grep -qs ' " + guestPath + " fuse.sshfs ' /proc/mounts", nil
}

// UnmountCommand fails only when the mount is still there afterwards.
func UnmountCommand(guestPath string) (string, error) {
	mounted, err := MountedCommand(guestPath)
	if err != nil {
		return "", err
	}
	return unmount(guestPath) + " || ! " + mounted, nil
}
