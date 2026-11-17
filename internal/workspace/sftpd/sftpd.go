// Package sftpd serves one workspace folder over SFTP to a passive sshfs
// in the guest, for hosts whose QEMU has no virtio-9p (native Windows).
// Every file operation goes through an os.Root, so neither "..", absolute
// paths, symlinks nor junctions can leave the folder.
package sftpd

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
)

func init() {
	// Without statvfs@openssh.com sshfs reports fixed large numbers for
	// df instead of failing statfs.
	if err := sftp.SetSFTPExtensions("posix-rename@openssh.com", "hardlink@openssh.com"); err != nil {
		panic(err)
	}
}

var errDenied = sftp.ErrSSHFxPermissionDenied

// Serve answers SFTP requests read from in until in reaches EOF or ctx
// is done, then closes out. "/" in requests is root.
func Serve(ctx context.Context, in io.Reader, out io.WriteCloser, root string, readOnly bool) error {
	defer out.Close()
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	h := &handler{root: r, readOnly: readOnly, deriveModes: deriveModes}
	rs := sftp.NewRequestServer(struct {
		io.Reader
		io.WriteCloser
	}{in, out}, sftp.Handlers{FileGet: h, FilePut: h, FileCmd: h, FileList: h})
	stop := context.AfterFunc(ctx, func() {
		rs.Close()
		if c, ok := in.(io.Closer); ok {
			c.Close()
		}
	})
	defer stop()
	err = rs.Serve()
	if errors.Is(err, io.EOF) || ctx.Err() != nil {
		return nil
	}
	return err
}

// deriveModes is set where the host file system keeps no permission
// bits beyond read-only, so the guest sees modes derived by derive.
var deriveModes = runtime.GOOS == "windows"

type handler struct {
	root        *os.Root
	readOnly    bool
	deriveModes bool

	mu   sync.Mutex
	exec map[string]execEntry
}

type execEntry struct {
	size  int64
	mtime time.Time
	exec  bool
}

// recovered turns a panic in a request handler into a failed request.
// The client is the guest, and pkg/sftp runs handlers without a recover,
// so a panic would otherwise take the whole daemon down.
func recovered(err *error) {
	if p := recover(); p != nil {
		log.Printf("sftpd: request panicked: %v\n%s", p, debug.Stack())
		*err = sftp.ErrSSHFxFailure
	}
}

// rel maps a request path to a name relative to the root.
func rel(p string) (string, error) {
	p = path.Clean("/" + p)
	if p == "/" {
		return ".", nil
	}
	p = p[1:]
	for c := range strings.SplitSeq(p, "/") {
		if !validName(c) {
			return "", errDenied
		}
	}
	return p, nil
}

// validName rejects names Windows would reinterpret: stream suffixes,
// device names and the dots and spaces Win32 strips. The checks apply on
// every OS so the behavior is the same wherever it is tested.
func validName(c string) bool {
	if c == "" || c == "." || c == ".." || strings.HasSuffix(c, ".") || strings.HasSuffix(c, " ") {
		return false
	}
	for _, r := range c {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`\:<>"|?*`, r) {
			return false
		}
	}
	base, _, _ := strings.Cut(c, ".")
	switch base = strings.ToUpper(strings.TrimRight(base, " ")); base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return false
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '0' && base[3] <= '9' {
		return false
	}
	return true
}

func (h *handler) Fileread(r *sftp.Request) (_ io.ReaderAt, err error) {
	defer recovered(&err)
	name, err := rel(r.Filepath)
	if err != nil {
		return nil, err
	}
	return h.root.Open(name)
}

func (h *handler) Filewrite(r *sftp.Request) (_ io.WriterAt, err error) {
	defer recovered(&err)
	return h.open(r, os.O_WRONLY)
}

func (h *handler) OpenFile(r *sftp.Request) (_ sftp.WriterAtReaderAt, err error) {
	defer recovered(&err)
	return h.open(r, os.O_RDWR)
}

// open ignores the append flag: sshfs writes at explicit offsets, and
// O_APPEND would break WriteAt. It also ignores the requested mode:
// pkg/sftp stores the open flags where the attribute flags belong, so the
// attributes of an open request cannot be decoded, and Windows keeps no
// permission bits beyond read-only anyway.
func (h *handler) open(r *sftp.Request, flag int) (*os.File, error) {
	if h.readOnly {
		return nil, errDenied
	}
	name, err := rel(r.Filepath)
	if err != nil {
		return nil, err
	}
	pf := r.Pflags()
	if pf.Creat {
		flag |= os.O_CREATE
	}
	if pf.Trunc {
		flag |= os.O_TRUNC
	}
	if pf.Excl {
		flag |= os.O_EXCL
	}
	return h.root.OpenFile(name, flag, 0o644)
}

func (h *handler) Filecmd(r *sftp.Request) (err error) {
	defer recovered(&err)
	if h.readOnly {
		return errDenied
	}
	if r.Method == "Symlink" {
		return h.symlink(r.Filepath, r.Target)
	}
	name, err := rel(r.Filepath)
	if err != nil {
		return err
	}
	if name == "." {
		return errDenied
	}
	switch r.Method {
	case "Setstat":
		return h.setstat(name, r)
	case "Rename", "PosixRename", "Link":
		target, err := rel(r.Target)
		if err != nil {
			return err
		}
		if target == "." {
			return errDenied
		}
		if err := h.checkMove(name, target); err != nil {
			return err
		}
		switch r.Method {
		case "Link":
			return h.root.Link(name, target)
		case "Rename":
			// SFTPv3 rename never replaces; sshfs uses posix-rename for that.
			if _, err := h.root.Lstat(target); err == nil {
				return os.ErrExist
			}
		}
		return h.root.Rename(name, target)
	case "Mkdir":
		perm := os.FileMode(0o755)
		if attrs := r.Attributes(); r.AttrFlags().Permissions && attrs != nil {
			perm = attrs.FileMode().Perm()
		}
		return h.root.Mkdir(name, perm)
	case "Rmdir", "Remove":
		fi, err := h.root.Lstat(name)
		if err != nil {
			return err
		}
		if fi.IsDir() != (r.Method == "Rmdir") {
			return sftp.ErrSSHFxFailure
		}
		return h.root.Remove(name)
	}
	return sftp.ErrSSHFxOpUnsupported
}

func (h *handler) PosixRename(r *sftp.Request) error { return h.Filecmd(r) }

func (h *handler) setstat(name string, r *sftp.Request) error {
	flags, attrs := r.AttrFlags(), r.Attributes()
	if attrs == nil {
		return sftp.ErrSSHFxBadMessage
	}
	if flags.UidGid {
		return sftp.ErrSSHFxOpUnsupported
	}
	if flags.Size {
		f, err := h.root.OpenFile(name, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		err = f.Truncate(int64(attrs.Size))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	if flags.Permissions {
		if err := h.root.Chmod(name, attrs.FileMode().Perm()); err != nil {
			return err
		}
	}
	if flags.Acmodtime {
		return h.root.Chtimes(name, time.Unix(int64(attrs.Atime), 0), time.Unix(int64(attrs.Mtime), 0))
	}
	return nil
}

// symlink accepts only relative targets that stay inside the root
// lexically, so the link is meaningful on both sides.
func (h *handler) symlink(target, link string) error {
	name, err := rel(link)
	if err != nil {
		return err
	}
	if name == "." || target == "" || strings.HasPrefix(target, "/") || escapes(path.Dir(name), target) {
		return errDenied
	}
	for c := range strings.SplitSeq(target, "/") {
		if c != "" && c != "." && c != ".." && !validName(c) {
			return errDenied
		}
	}
	if err := h.realParents(name); err != nil {
		return err
	}
	return h.root.Symlink(target, name)
}

// escapes reports whether target, relative to dir, can resolve outside
// the root: by climbing above it, or by a ".." after a name, which could
// climb out through a symlink that name resolves to.
func escapes(dir, target string) bool {
	depth := 0
	if dir != "." {
		depth = strings.Count(dir, "/") + 1
	}
	leading := true
	for c := range strings.SplitSeq(target, "/") {
		switch c {
		case "", ".":
		case "..":
			if depth--; !leading || depth < 0 {
				return true
			}
		default:
			leading = false
		}
	}
	return false
}

// realParents denies paths below a symlink: os.Root would follow it, so a
// relative link target would be judged at the wrong depth.
func (h *handler) realParents(name string) error {
	for d := path.Dir(name); d != "."; d = path.Dir(d) {
		fi, err := h.root.Lstat(d)
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return errDenied
		}
	}
	return nil
}

// checkMove denies a rename or hard link that would put a relative
// symlink, or one inside a moved directory, where its target escapes.
func (h *handler) checkMove(from, to string) error {
	if err := h.realParents(to); err != nil {
		return err
	}
	fi, err := h.root.Lstat(from)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return h.checkLinkAt(from, to)
	}
	if !fi.IsDir() {
		return nil
	}
	return fs.WalkDir(h.root.FS(), from, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return h.checkLinkAt(p, to+p[len(from):])
		}
		return nil
	})
}

func (h *handler) checkLinkAt(link, at string) error {
	target, err := h.root.Readlink(link)
	if err != nil {
		return err
	}
	target = filepath.ToSlash(target)
	if filepath.IsAbs(target) || filepath.VolumeName(target) != "" || strings.HasPrefix(target, "/") {
		return nil // a move does not change where it points
	}
	if escapes(path.Dir(at), target) {
		return errDenied
	}
	return nil
}

func (h *handler) Filelist(r *sftp.Request) (_ sftp.ListerAt, err error) {
	defer recovered(&err)
	name, err := rel(r.Filepath)
	if err != nil {
		return nil, err
	}
	switch r.Method {
	case "List":
		f, err := h.root.Open(name)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		fis, err := f.Readdir(-1)
		if err != nil {
			return nil, err
		}
		for i, fi := range fis {
			fis[i] = h.derive(path.Join(name, fi.Name()), fi)
		}
		return lister(fis), nil
	case "Stat":
		fi, err := h.root.Stat(name)
		if err != nil {
			return nil, err
		}
		return lister{h.derive(name, fi)}, nil
	}
	return nil, sftp.ErrSSHFxOpUnsupported
}

func (h *handler) Lstat(r *sftp.Request) (_ sftp.ListerAt, err error) {
	defer recovered(&err)
	name, err := rel(r.Filepath)
	if err != nil {
		return nil, err
	}
	fi, err := h.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	return lister{h.derive(name, fi)}, nil
}

// derive reports directories as 755 and regular files as 755 when they
// start with an ELF header or "#!", else 644, the way Cygwin does: Windows
// keeps no execute bit, so a binary built in the guest would otherwise
// not run. The read-only attribute clears the write bits. chmod cannot
// change the execute bit on such a host.
func (h *handler) derive(name string, fi os.FileInfo) os.FileInfo {
	if !h.deriveModes {
		return fi
	}
	m := fi.Mode()
	perm := fs.FileMode(0o755)
	switch {
	case m.IsDir():
	case m.IsRegular():
		if !h.executable(name, fi) {
			perm = 0o644
		}
	default:
		return fi
	}
	if m&0o200 == 0 {
		perm &^= 0o222
	}
	return modeInfo{fi, m&^fs.ModePerm | perm}
}

// maxExecEntries bounds the cache; past it the cache starts over.
const maxExecEntries = 1 << 16

// executable reads the first bytes of name once per size and mtime, since
// sshfs runs with attribute caching off and stats files constantly.
func (h *handler) executable(name string, fi os.FileInfo) bool {
	h.mu.Lock()
	e, ok := h.exec[name]
	h.mu.Unlock()
	if ok && e.size == fi.Size() && e.mtime.Equal(fi.ModTime()) {
		return e.exec
	}
	var head [4]byte
	n := 0
	if f, err := h.root.Open(name); err == nil {
		n, _ = io.ReadFull(f, head[:])
		f.Close()
	}
	exec := n >= 2 && string(head[:2]) == "#!" || n == 4 && string(head[:]) == "\x7fELF"
	h.mu.Lock()
	if h.exec == nil || len(h.exec) >= maxExecEntries {
		h.exec = make(map[string]execEntry)
	}
	h.exec[name] = execEntry{fi.Size(), fi.ModTime(), exec}
	h.mu.Unlock()
	return exec
}

type modeInfo struct {
	os.FileInfo
	mode fs.FileMode
}

func (fi modeInfo) Mode() fs.FileMode { return fi.mode }

// Readlink hides absolute targets such as junctions: they would reveal
// host paths and cannot resolve in the guest anyway.
func (h *handler) Readlink(p string) (_ string, err error) {
	defer recovered(&err)
	name, err := rel(p)
	if err != nil {
		return "", err
	}
	target, err := h.root.Readlink(name)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(target) || filepath.VolumeName(target) != "" || strings.HasPrefix(target, `\`) || strings.HasPrefix(target, "/") {
		return "", errDenied
	}
	return filepath.ToSlash(target), nil
}

type lister []os.FileInfo

func (l lister) ListAt(dst []os.FileInfo, off int64) (int, error) {
	if off >= int64(len(l)) {
		return 0, io.EOF
	}
	n := copy(dst, l[off:])
	if n < len(dst) {
		return n, io.EOF
	}
	return n, nil
}
