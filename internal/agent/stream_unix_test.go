//go:build linux

package agent

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// The agent runs as root and must not connect a socket another guest user
// could have planted.
func TestDialUserSocketChecksOwnership(t *testing.T) {
	uid := os.Getuid()
	if uid <= 0 {
		t.Skip("needs a non-root uid")
	}
	root := t.TempDir()
	dir := filepath.Join(root, "run", "user", strconv.Itoa(uid), "pvm")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	os.Chmod(filepath.Dir(dir), 0o700)
	sock := filepath.Join(dir, "xpra.sock")
	if len(sock) >= 108 {
		t.Skip("temp dir too long for a unix socket")
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	os.Chmod(sock, 0o600)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	c, err := dialUserSocket(root, uid, xpraSocket)
	if err != nil {
		t.Fatalf("owned socket refused: %v", err)
	}
	c.Close()

	if _, err := dialUserSocket(root, uid+1, xpraSocket); err == nil {
		t.Error("connected another uid's runtime dir")
	}
	if _, err := dialUserSocket(root, 0, xpraSocket); err == nil {
		t.Error("connected root's runtime dir")
	}
	os.Chmod(dir, 0o755)
	if _, err := dialUserSocket(root, uid, xpraSocket); err == nil {
		t.Error("connected through a directory other users can read")
	}
	os.Chmod(dir, 0o700)

	// a symlinked directory is refused even when it points somewhere owned
	other := filepath.Join(root, "elsewhere")
	os.Rename(dir, other)
	if err := os.Symlink(other, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := dialUserSocket(root, uid, xpraSocket); err == nil {
		t.Error("connected through a symlinked directory")
	}
}
