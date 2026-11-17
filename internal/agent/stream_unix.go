//go:build !windows

package agent

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// appDisplay is the X display of the pvm app server.
const appDisplay = ":100"

// xpraSocket is the VM user's Xpra server socket, relative to its
// runtime directory.
const xpraSocket = "pvm/xpra.sock"

// dialUserSocket connects the unix socket rel under uid's runtime
// directory. The agent runs as root, so the directory and socket must
// belong to uid; another guest user cannot plant one.
func dialUserSocket(root string, uid int, rel string) (io.ReadWriteCloser, error) {
	if uid < 1000 {
		return nil, fmt.Errorf("uid %d is not a login user", uid)
	}
	runtime := filepath.Join(root, "run", "user", strconv.Itoa(uid))
	path := filepath.Join(runtime, rel)
	for _, p := range []string{runtime, filepath.Dir(path)} {
		if err := checkOwned(p, uid, fs.ModeDir, 0o077); err != nil {
			return nil, err
		}
	}
	if err := checkOwned(path, uid, fs.ModeSocket, 0o077); err != nil {
		return nil, err
	}
	return net.DialTimeout("unix", path, 5*time.Second)
}

// checkOwned requires path to be a non-symlink of the given type owned by
// uid, with none of the deny permission bits set.
func checkOwned(path string, uid int, typ fs.FileMode, deny fs.FileMode) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Type() != typ {
		return fmt.Errorf("%s is not a %v", path, typ)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != uid {
		return fmt.Errorf("%s is not owned by uid %d", path, uid)
	}
	if fi.Mode().Perm()&deny != 0 {
		return fmt.Errorf("%s is accessible to other users (mode %v)", path, fi.Mode().Perm())
	}
	return nil
}

// maxClipboard bounds one clipboard transfer into the guest.
const maxClipboard = 16 << 20

// clipMIME maps a clipboard target kind to the X selection type.
var clipMIME = map[string]string{"text": "UTF8_STRING", "png": "image/png"}

// clipUser looks up a login user for the clipboard targets.
func clipUser(uid int) (*user.User, error) {
	if uid < 1000 {
		return nil, fmt.Errorf("uid %d is not a login user", uid)
	}
	return user.LookupId(strconv.Itoa(uid))
}

// asUser runs argv as u on the pvm app display.
func asUser(ctx context.Context, u *user.User, extraEnv []string, argv ...string) *exec.Cmd {
	args := append([]string{"-u", u.Username, "--", "env", "DISPLAY=" + appDisplay, "HOME=" + u.HomeDir, "XDG_RUNTIME_DIR=/run/user/" + u.Uid}, extraEnv...)
	return exec.CommandContext(ctx, "runuser", append(args, argv...)...)
}

// clipboardSink sets uid's clipboard on the pvm app display from what the
// host writes, as that user, through xclip. xclip keeps serving the
// selection in the background after the stream ends.
func clipboardSink(uid int, kind string) (io.ReadWriteCloser, error) {
	mime, ok := clipMIME[kind]
	if !ok && kind != "files" {
		return nil, fmt.Errorf("unknown clipboard kind %q", kind)
	}
	u, err := clipUser(uid)
	if err != nil {
		return nil, err
	}
	var cmd *exec.Cmd
	limit := int64(maxClipboard)
	if kind == "files" {
		cmd = asUser(context.Background(), u, []string{"PVM_CLIP_FILES_PY=" + clipFilesPy}, "sh", "-c", clipFilesScript)
		limit = maxClipFiles
	} else {
		cmd = asUser(context.Background(), u, nil, "xclip", "-selection", "clipboard", "-t", mime, "-i")
	}
	r, w := io.Pipe()
	// past the limit tar sees a cut archive and fails
	cmd.Stdin = io.LimitReader(r, limit)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start xclip: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); r.Close() }()
	return &clipConn{w: w, done: done, closed: make(chan struct{})}, nil
}

// clipConn takes the clipboard content. Reads block until the host has
// closed its side and xclip has taken the selection, then report EOF.
type clipConn struct {
	w      *io.PipeWriter
	done   chan error
	closed chan struct{}
	once   sync.Once
}

func (c *clipConn) Write(p []byte) (int, error) { return c.w.Write(p) }

func (c *clipConn) closeInput() {
	c.once.Do(func() {
		c.w.Close()
		close(c.closed)
	})
}

func (c *clipConn) Read([]byte) (int, error) {
	<-c.closed
	if err := <-c.done; err != nil {
		c.done <- err
		return 0, fmt.Errorf("set clipboard: %w", err)
	}
	c.done <- nil
	return 0, io.EOF
}

func (c *clipConn) CloseWrite() error { c.closeInput(); return nil }
func (c *clipConn) Close() error      { c.closeInput(); return nil }
