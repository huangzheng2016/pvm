// Package guest builds the shell commands pvm runs inside a VM to manage a
// workspace, and parses their output. Every command is a single string for
// "ssh <target> <command>"; the script runs under bash whatever the login
// shell is, and arguments are passed as positional parameters, never spliced
// into the script. Relative paths go over stdin, NUL-separated.
package guest

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/huangzheng2016/pvm/internal/workspace"
)

// Skipped is a guest path that is not a file, directory or symlink and is
// therefore never transferred.
type Skipped struct {
	Path   string
	Reason string
}

// PathState is what ParseState reports about a guest workspace path.
type PathState string

const (
	StateAbsent   PathState = "absent"
	StateEmpty    PathState = "empty"
	StateNonEmpty PathState = "nonempty"
	StateNotDir   PathState = "notdir"
)

const prologue = `set -euo pipefail
export LC_ALL=C
die() { printf 'pvm: %s\n' "$*" >&2; exit 1; }
`

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func command(script string, args ...string) string {
	var b strings.Builder
	b.WriteString("bash -c ")
	b.WriteString(quote(prologue + script))
	b.WriteString(" pvm")
	for _, a := range args {
		b.WriteByte(' ')
		b.WriteString(quote(a))
	}
	return b.String()
}

func checkPath(guestPath string) error {
	id, ok := strings.CutPrefix(guestPath, workspace.GuestRoot+"/")
	if !ok || workspace.ValidID(id) != nil || workspace.GuestPathFor(id) != guestPath {
		return fmt.Errorf("invalid guest workspace path %q", guestPath)
	}
	return nil
}

const prepareScript = `root=$2
[ ! -L "$root" ] || die "$root is a symlink"
if [ ! -d "$root" ] || [ "$(stat -c %u -- "$root")" != "$(id -u)" ]; then
	sudo -n install -d -o "$(id -un)" -g "$(id -gn)" -m 0755 -- "$root"
fi
mkdir -p -- "$1" "$3"
link=$HOME/workspace
if [ ! -e "$link" ] && [ ! -L "$link" ]; then
	ln -s -- "$root" "$link"
elif [ "$(readlink -- "$link")" != "$root" ]; then
	echo "note: ~/workspace already exists and is not a link to $root; workspaces are in $root"
fi
`

// PrepareCommand makes /workspace owned by the login user (with sudo only
// when needed), then creates guestPath and the staging directory. It also
// links ~/workspace to /workspace, which is easier to find but stays a
// link so tools that walk the home directory do not descend into mounts.
// Anything it prints is a note for the user.
func PrepareCommand(guestPath string) (string, error) {
	if err := checkPath(guestPath); err != nil {
		return "", err
	}
	return command(prepareScript, guestPath, workspace.GuestRoot, workspace.StagingDir), nil
}

const stateScript = `p=$1
if [ -L "$p" ] || { [ -e "$p" ] && [ ! -d "$p" ]; }; then echo notdir
elif [ ! -e "$p" ]; then echo absent
elif [ -z "$(find "$p" -mindepth 1 -maxdepth 1 -print -quit)" ]; then echo empty
else echo nonempty
fi
`

// ExistsNonEmptyCommand prints the PathState of guestPath; read it with
// ParseState.
func ExistsNonEmptyCommand(guestPath string) (string, error) {
	if err := checkPath(guestPath); err != nil {
		return "", err
	}
	return command(stateScript, guestPath), nil
}

func ParseState(out []byte) (PathState, error) {
	switch s := PathState(strings.TrimSpace(string(out))); s {
	case StateAbsent, StateEmpty, StateNonEmpty, StateNotDir:
		return s, nil
	}
	return "", fmt.Errorf("unexpected guest path state %q", strings.TrimSpace(string(out)))
}

const listScript = `cd -- "$1"
find . -mindepth 1 -printf '%y\0%s\0%T@\0%m\0%l\0%P\0'
`

// ListCommand prints one NUL-delimited record per path below guestPath;
// read it with ParseList.
func ListCommand(guestPath string) (string, error) {
	if err := checkPath(guestPath); err != nil {
		return "", err
	}
	return command(listScript, guestPath), nil
}

var skipReasons = map[string]string{
	"b": "block device",
	"c": "character device",
	"p": "FIFO",
	"s": "socket",
	"D": "door",
}

// ParseList parses ListCommand output. SHA256 is left empty; files that
// need one are hashed with HashCommand.
func ParseList(out []byte) (workspace.Manifest, []Skipped, error) {
	fields := bytes.Split(out, []byte{0})
	if len(fields) == 0 || len(fields[len(fields)-1]) != 0 || (len(fields)-1)%6 != 0 {
		return nil, nil, fmt.Errorf("malformed guest file list")
	}
	m := workspace.Manifest{}
	var skipped []Skipped
	for i := 0; i+6 <= len(fields)-1; i += 6 {
		typ, size, mtime, mode, link, rel := string(fields[i]), string(fields[i+1]), string(fields[i+2]), string(fields[i+3]), string(fields[i+4]), string(fields[i+5])
		if err := checkRel(rel); err != nil {
			return nil, nil, err
		}
		if !utf8.ValidString(rel) {
			// The baseline is JSON, which cannot hold such a name.
			skipped = append(skipped, Skipped{Path: rel, Reason: "name is not valid UTF-8"})
			continue
		}
		e := workspace.Entry{Path: rel}
		switch typ {
		case "d":
			e.Type = workspace.TypeDir
		case "l":
			e.Type = workspace.TypeSymlink
			e.Link = link
		case "f":
			e.Type = workspace.TypeFile
			n, err := strconv.ParseInt(size, 10, 64)
			if err != nil || n < 0 {
				return nil, nil, fmt.Errorf("guest file list: bad size %q for %s", size, rel)
			}
			e.Size = n
			if e.MTime, err = parseMTime(mtime); err != nil {
				return nil, nil, fmt.Errorf("guest file list: bad mtime %q for %s", mtime, rel)
			}
			perm, err := strconv.ParseUint(mode, 8, 32)
			if err != nil {
				return nil, nil, fmt.Errorf("guest file list: bad mode %q for %s", mode, rel)
			}
			e.Exec = perm&0o111 != 0
		default:
			reason := skipReasons[typ]
			if reason == "" {
				reason = "unsupported file type " + strconv.Quote(typ)
			}
			skipped = append(skipped, Skipped{Path: rel, Reason: reason})
			continue
		}
		if _, dup := m[rel]; dup {
			return nil, nil, fmt.Errorf("guest file list: duplicate path %s", rel)
		}
		m[rel] = e
	}
	return m, skipped, nil
}

func checkRel(rel string) error {
	if rel == "" || strings.HasPrefix(rel, "/") {
		return fmt.Errorf("guest file list: invalid path %q", rel)
	}
	for _, c := range strings.Split(rel, "/") {
		if c == "" || c == "." || c == ".." {
			return fmt.Errorf("guest file list: invalid path %q", rel)
		}
	}
	return nil
}

// parseMTime converts find's %T@ ("seconds.fraction") to unix nanoseconds.
func parseMTime(s string) (int64, error) {
	neg := strings.HasPrefix(s, "-")
	sec, frac, _ := strings.Cut(strings.TrimPrefix(s, "-"), ".")
	n, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		return 0, err
	}
	frac = (frac + "000000000")[:9]
	ns, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0, err
	}
	t := n*1e9 + ns
	if neg {
		t = -t
	}
	return t, nil
}

const hashScript = `cd -- "$1"
xargs -0 -r sha256sum -z --
`

// HashCommand reads NUL-separated relative paths on stdin and prints their
// SHA-256 sums; read it with ParseHashes.
func HashCommand(guestPath string) (string, error) {
	if err := checkPath(guestPath); err != nil {
		return "", err
	}
	return command(hashScript, guestPath), nil
}

// ParseHashes parses "sha256sum -z" output, which neither escapes names nor
// ends lines with newlines.
func ParseHashes(out []byte) (map[string]string, error) {
	recs := bytes.Split(out, []byte{0})
	if len(recs[len(recs)-1]) != 0 {
		return nil, fmt.Errorf("malformed guest hash output")
	}
	sums := map[string]string{}
	for _, r := range recs[:len(recs)-1] {
		if len(r) < 67 || r[64] != ' ' || (r[65] != ' ' && r[65] != '*') {
			return nil, fmt.Errorf("malformed guest hash line %q", r)
		}
		sum := string(r[:64])
		for _, c := range sum {
			if !strings.ContainsRune("0123456789abcdef", c) {
				return nil, fmt.Errorf("malformed guest hash line %q", r)
			}
		}
		sums[string(r[66:])] = sum
	}
	return sums, nil
}

// chk rejects relative paths that are empty, absolute, non-canonical or
// that pass through a symlink inside the workspace $p.
const chkFunc = `chk() {
	case "/$1/" in //*|*//*|*/./*|*/../*) die "invalid relative path: $1";; esac
	local r=$1 d=$p
	while [[ $r == */* ]]; do
		d=$d/${r%%/*} r=${r#*/}
		[ ! -L "$d" ] || die "path crosses a symlink: $1"
	done
}
`

const uploadPrologue = `p=$1 st=$2
` + chkFunc + `[ ! -L "$p" ] || die "$p is a symlink"
mkdir -p -- "$st"
tmp=$(mktemp -d -- "$st/up.XXXXXXXX")
trap 'rm -rf -- "$tmp"' EXIT
tar -x --no-same-owner -f - -C "$tmp"
mkdir -p -- "$tmp/files"
`

const uploadFreshScript = uploadPrologue + `if [ -e "$p" ]; then
	[ -d "$p" ] || die "$p is not a directory"
	[ -z "$(find "$p" -mindepth 1 -maxdepth 1 -print -quit)" ] || die "$p is not empty"
fi
mv -T -- "$tmp/files" "$p"
`

const uploadPushScript = uploadPrologue + `[ -d "$p" ] || die "$p does not exist"
rmd() { rmdir -- "$p/$1" || die "cannot remove directory $1: it holds files outside the workspace"; }
if [ -f "$tmp/pvm/deletes" ]; then
	while IFS= read -r -d '' rel; do
		chk "$rel"
		t=$p/$rel
		if [ -d "$t" ] && [ ! -L "$t" ]; then rmd "$rel"
		elif [ -L "$t" ] || [ -e "$t" ]; then rm -f -- "$t"
		fi
	done < "$tmp/pvm/deletes"
fi
(cd -- "$tmp/files" && find . -mindepth 1 -printf '%y%P\0') > "$tmp/list"
while IFS= read -r -d '' ent; do
	y=${ent:0:1} rel=${ent:1}
	chk "$rel"
	t=$p/$rel
	if [ "$y" = d ]; then
		if [ -L "$t" ] || { [ -e "$t" ] && [ ! -d "$t" ]; }; then rm -f -- "$t"; fi
		mkdir -p -- "$t"
	else
		if [ -d "$t" ] && [ ! -L "$t" ]; then rmd "$rel"; fi
		mkdir -p -- "${t%/*}"
		mv -fT -- "$tmp/files/$rel" "$t"
	fi
done < "$tmp/list"
`

// UploadCommand extracts a tar from stdin into a private staging directory
// and applies it. Entries under "files/" are the new contents; an optional
// regular file "pvm/deletes" lists NUL-separated relative paths to delete,
// deepest first.
//
// With fresh set, guestPath must be absent or an empty directory and is
// replaced by the staged tree in one rename, so an interrupted upload
// leaves it untouched. Otherwise the deletions are applied first, then every
// staged file and symlink is renamed into place and staged directories are
// created; a path whose type changes is removed first. Directory modes and
// mtimes are not preserved.
func UploadCommand(guestPath string, fresh bool) (string, error) {
	if err := checkPath(guestPath); err != nil {
		return "", err
	}
	return uploadCmd(guestPath, workspace.StagingDir, fresh), nil
}

func uploadCmd(guestPath, staging string, fresh bool) string {
	if fresh {
		return command(uploadFreshScript, guestPath, staging)
	}
	return command(uploadPushScript, guestPath, staging)
}

const renameScript = `p=$1
` + chkFunc + `[ -d "$p" ] && [ ! -L "$p" ] || die "$p does not exist"
while IFS= read -r -d '' from && IFS= read -r -d '' to; do
	chk "$from"
	chk "$to"
	if [ -L "$p/$to" ] || [ -e "$p/$to" ]; then die "cannot rename $from: $to exists"; fi
	mv -T -- "$p/$from" "$p/$to"
done
`

// RenameCommand reads NUL-separated (from, to) pairs of relative paths on
// stdin and renames each inside guestPath, refusing to overwrite.
func RenameCommand(guestPath string) (string, error) {
	if err := checkPath(guestPath); err != nil {
		return "", err
	}
	return command(renameScript, guestPath), nil
}

const downloadScript = `cd -- "$1"
tar -c --format=pax --null --verbatim-files-from --no-recursion --hard-dereference -T - -f -
`

// DownloadCommand reads NUL-separated relative paths on stdin and writes a
// tar of exactly those paths to stdout, hard links stored as plain files.
func DownloadCommand(guestPath string) (string, error) {
	if err := checkPath(guestPath); err != nil {
		return "", err
	}
	return command(downloadScript, guestPath), nil
}

const freeScript = `d=$1
[ -d "$d" ] || d=/
df -B1 --output=avail -- "$d" | tail -n 1
`

// FreeSpaceCommand prints the bytes available on the file system that
// holds /workspace, or / before /workspace exists.
func FreeSpaceCommand() string { return command(freeScript, workspace.GuestRoot) }

func ParseFreeSpace(out []byte) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("unexpected guest free space %q", strings.TrimSpace(string(out)))
	}
	return n, nil
}

// A mount anywhere below the guest copy may be a host folder, which rm
// must never reach. Workspace paths need no mountinfo unescaping.
const removeScript = `m=$(awk -v p="$1" '$5 == p || index($5, p "/") == 1 { print $5 }' /proc/self/mountinfo)
[ -z "$m" ] || die "refusing to delete $1: mounted below it: $m"
rm -rf --one-file-system -- "$1"
`

// RemoveCommand deletes the guest copy of a workspace, refusing when
// anything is mounted at or below it.
func RemoveCommand(guestPath string) (string, error) {
	if err := checkPath(guestPath); err != nil {
		return "", err
	}
	return command(removeScript, guestPath), nil
}

const mountedScript = `if mountpoint -q -- "$1"; then echo yes; else echo no; fi
`

// MountedCommand prints yes or no; read it with ParseMounted.
func MountedCommand(guestPath string) (string, error) {
	if err := checkPath(guestPath); err != nil {
		return "", err
	}
	return command(mountedScript, guestPath), nil
}

func ParseMounted(out []byte) (bool, error) {
	switch s := strings.TrimSpace(string(out)); s {
	case "yes":
		return true, nil
	case "no":
		return false, nil
	default:
		return false, fmt.Errorf("unexpected mount state %q", s)
	}
}
