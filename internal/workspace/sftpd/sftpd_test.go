package sftpd

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/pkg/sftp"
)

// fixture lays out base/root (served) and base/outside (must stay
// unreachable) and returns a client connected to Serve.
func fixture(t *testing.T, readOnly bool) (c *sftp.Client, root, outside string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "root")
	outside = filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c2sR, c2sW := io.Pipe()
	s2cR, s2cW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, c2sR, s2cW, root, readOnly) }()
	c, err := sftp.NewClientPipe(s2cR, c2sW)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c.Close()
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return c, root, outside
}

func writeFile(t *testing.T, c *sftp.Client, name, data string) {
	t.Helper()
	f, err := c.Create(name)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if _, err := f.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func readFile(c *sftp.Client, name string) (string, error) {
	f, err := c.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	return string(b), err
}

func TestServeReadWrite(t *testing.T) {
	c, root, _ := fixture(t, false)
	writeFile(t, c, "/a.txt", "hello")
	if got, err := readFile(c, "/a.txt"); err != nil || got != "hello" {
		t.Fatalf("read back = %q, %v", got, err)
	}
	if err := c.Mkdir("/d"); err != nil {
		t.Fatal(err)
	}
	if err := c.Rename("/a.txt", "/d/b.txt"); err != nil {
		t.Fatal(err)
	}
	writeFile(t, c, "/c.txt", "c")
	if err := c.Rename("/c.txt", "/d/b.txt"); err == nil {
		t.Fatal("rename replaced an existing file")
	}
	if err := c.PosixRename("/c.txt", "/d/b.txt"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "d", "b.txt")); err != nil || string(b) != "c" {
		t.Fatalf("host file = %q, %v", b, err)
	}
	fis, err := c.ReadDir("/d")
	if err != nil || len(fis) != 1 || fis[0].Name() != "b.txt" {
		t.Fatalf("ReadDir = %v, %v", fis, err)
	}
	if err := c.Truncate("/d/b.txt", 0); err != nil {
		t.Fatal(err)
	}
	if fi, err := c.Stat("/d/b.txt"); err != nil || fi.Size() != 0 {
		t.Fatalf("Stat after truncate = %v, %v", fi, err)
	}
	if err := c.Remove("/d"); err == nil {
		t.Fatal("Remove deleted a directory")
	}
	if err := c.Remove("/d/b.txt"); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveDirectory("/d"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "d")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory still there: %v", err)
	}
}

func TestServeEscapesStayInside(t *testing.T) {
	c, root, outside := fixture(t, false)
	writeFile(t, c, "/../../escape.txt", "x")
	if _, err := os.Stat(filepath.Join(root, "escape.txt")); err != nil {
		t.Fatalf("cleaned path did not land in root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "escape.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file written outside root: %v", err)
	}
	for _, p := range []string{
		"../outside/secret.txt",
		"/../outside/secret.txt",
		"/../../../../../../../../../../" + filepath.ToSlash(outside) + "/secret.txt",
		filepath.ToSlash(filepath.Join(outside, "secret.txt")),
		filepath.Join(outside, "secret.txt"),
	} {
		if got, err := readFile(c, p); err == nil || got == "secret" {
			t.Errorf("read %q = %q, %v; want error", p, got, err)
		}
	}
}

func TestServeRejectsWindowsNames(t *testing.T) {
	c, _, _ := fixture(t, false)
	for _, name := range []string{
		"a.txt:stream", "a.txt::$DATA", "CON", "con", "aux.c", "NUL.tar.gz", "nul .txt", "COM1", "lpt9.log", "conin$",
		"x.", "x ", "a<b", "a>b", `a"b`, "a|b", "a?b", "a*b", "tab\tname",
	} {
		if _, err := c.Create("/" + name); !errors.Is(err, os.ErrPermission) {
			t.Errorf("create %q = %v, want permission denied", name, err)
		}
		if err := c.Mkdir("/" + name); !errors.Is(err, os.ErrPermission) {
			t.Errorf("mkdir %q = %v, want permission denied", name, err)
		}
	}
	// pkg/sftp turns backslashes into separators on Windows before the
	// handler sees them, so only the name check itself is testable there.
	if validName(`a\b`) {
		t.Error(`validName("a\b") = true`)
	}
	for _, ok := range []string{"console.log", "com10", "lpt", "a.b.c", "~x", "x~1"} {
		if !validName(ok) {
			t.Errorf("validName(%q) = false", ok)
		}
	}
}

func TestServeReadOnly(t *testing.T) {
	c, root, _ := fixture(t, true)
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, err := readFile(c, "/f.txt"); err != nil || got != "data" {
		t.Fatalf("read = %q, %v", got, err)
	}
	checks := map[string]error{}
	_, checks["create"] = c.Create("/new.txt")
	_, checks["open for write"] = c.OpenFile("/f.txt", os.O_WRONLY)
	checks["mkdir"] = c.Mkdir("/n")
	checks["remove"] = c.Remove("/f.txt")
	checks["rmdir"] = c.RemoveDirectory("/d")
	checks["rename"] = c.Rename("/f.txt", "/g.txt")
	checks["posix-rename"] = c.PosixRename("/f.txt", "/g.txt")
	checks["chmod"] = c.Chmod("/f.txt", 0o600)
	checks["truncate"] = c.Truncate("/f.txt", 0)
	checks["symlink"] = c.Symlink("f.txt", "/l")
	checks["link"] = c.Link("/f.txt", "/h.txt")
	for op, err := range checks {
		if !errors.Is(err, os.ErrPermission) {
			t.Errorf("%s = %v, want permission denied", op, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(root, "f.txt")); err != nil || string(b) != "data" {
		t.Fatalf("file changed: %q, %v", b, err)
	}
	ents, _ := os.ReadDir(root)
	if names := dirNames(ents); !slices.Equal(names, []string{"d", "f.txt"}) {
		t.Fatalf("root = %v", names)
	}
}

func dirNames(ents []os.DirEntry) []string {
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestServeSymlinks(t *testing.T) {
	c, root, outside := fixture(t, false)
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "abs")); err != nil {
		t.Skipf("cannot create symlinks: %v", err)
	}
	if err := os.Symlink(filepath.Join("..", "outside", "secret.txt"), filepath.Join(root, "rel")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dir")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/abs", "/rel", "/dir/secret.txt"} {
		if got, err := readFile(c, p); err == nil || got == "secret" {
			t.Errorf("read %q = %q, %v; want error", p, got, err)
		}
	}
	if fis, err := c.ReadDir("/dir"); err == nil {
		t.Errorf("listed outside directory: %v", fis)
	}
	if target, err := c.ReadLink("/abs"); err == nil {
		t.Errorf("ReadLink revealed absolute target %q", target)
	}
	if target, err := c.ReadLink("/rel"); err != nil || target != "../outside/secret.txt" {
		t.Errorf("ReadLink(rel) = %q, %v", target, err)
	}
	for _, target := range []string{"../outside/secret.txt", "/etc/passwd", "a/../../x", `..\x`, "C:/x"} {
		if err := c.Symlink(target, "/new"); !errors.Is(err, os.ErrPermission) {
			t.Errorf("symlink to %q = %v, want permission denied", target, err)
		}
	}
	writeFile(t, c, "/f.txt", "inside")
	if err := c.Mkdir("/sub"); err != nil {
		t.Fatal(err)
	}
	if err := c.Symlink("../f.txt", "/sub/link"); err != nil {
		t.Fatal(err)
	}
	if got, err := readFile(c, "/sub/link"); err != nil || got != "inside" {
		t.Fatalf("read through inside link = %q, %v", got, err)
	}
	if fi, err := c.Lstat("/sub/link"); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("Lstat = %v, %v", fi, err)
	}

	// Links must not reach a shallower depth than their target allows,
	// whether moved alone, inside a directory, or created below a symlink.
	if err := c.Symlink("sub/../../x", "/dotdot"); !errors.Is(err, os.ErrPermission) {
		t.Errorf("'..' after a name = %v", err)
	}
	if err := c.Mkdir("/sub/deep"); err != nil {
		t.Fatal(err)
	}
	if err := c.Symlink("../../f.txt", "/sub/deep/up2"); err != nil {
		t.Fatal(err)
	}
	if err := c.Symlink(".", "/loop"); err != nil {
		t.Fatal(err)
	}
	denied := map[string]error{
		"rename link":       c.Rename("/sub/link", "/link"),
		"posix-rename link": c.PosixRename("/sub/link", "/link"),
		"hard link":         c.Link("/sub/link", "/link"),
		"rename dir":        c.Rename("/sub/deep", "/deep"),
		"link below link":   c.Symlink("../x", "/loop/l"),
		"move below link":   c.Rename("/f.txt", "/loop/g.txt"),
	}
	for name, err := range denied {
		if !errors.Is(err, os.ErrPermission) {
			t.Errorf("%s = %v, want permission denied", name, err)
		}
	}
	for _, p := range []string{"link", "deep", "l", "g.txt"} {
		if _, err := os.Lstat(filepath.Join(root, p)); !os.IsNotExist(err) {
			t.Errorf("%s exists after a denied request: %v", p, err)
		}
	}
	// Moves that keep the depth, or go deeper, are fine.
	if err := c.Rename("/sub/link", "/sub/deep/link2"); err != nil {
		t.Errorf("rename deeper: %v", err)
	}
	if err := c.Rename("/sub/deep", "/sub/deep2"); err != nil {
		t.Errorf("rename at the same depth: %v", err)
	}
}

// git opens its logs with O_APPEND; pkg/sftp leaves the open flags where
// the attribute flags belong, and the append bit reads as "permissions".
func TestServeOpenAppendCreate(t *testing.T) {
	c, root, _ := fixture(t, false)
	f, err := c.OpenFile("/HEAD.log", os.O_WRONLY|os.O_APPEND|os.O_CREATE)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("entry\n")); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "HEAD.log")); err != nil || string(b) != "entry\n" {
		t.Fatalf("HEAD.log = %q, %v", b, err)
	}
}

func TestRecoveredFailsTheRequest(t *testing.T) {
	err := func() (err error) {
		defer recovered(&err)
		var p *int
		return errors.New(strconv.Itoa(*p))
	}()
	if !errors.Is(err, sftp.ErrSSHFxFailure) {
		t.Fatalf("err = %v, want ErrSSHFxFailure", err)
	}
}

// Windows keeps no execute bit, so a binary built in the guest must still
// show up as executable.
func TestServeDerivesModes(t *testing.T) {
	old := deriveModes
	deriveModes = true
	t.Cleanup(func() { deriveModes = old })
	c, root, _ := fixture(t, false)
	files := map[string]string{"bin": "\x7fELF\x02\x01", "run.sh": "#!/bin/sh\n", "a.txt": "hello", "empty": ""}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "dir"), 0o700); err != nil {
		t.Fatal(err)
	}
	want := map[string]os.FileMode{"bin": 0o755, "run.sh": 0o755, "a.txt": 0o644, "empty": 0o644, "dir": os.ModeDir | 0o755}
	for name, mode := range want {
		for _, stat := range []func(string) (os.FileInfo, error){c.Stat, c.Lstat} {
			fi, err := stat("/" + name)
			if err != nil {
				t.Fatal(err)
			}
			if fi.Mode() != mode {
				t.Errorf("%s: mode %v, want %v", name, fi.Mode(), mode)
			}
		}
	}
	fis, err := c.ReadDir("/")
	if err != nil {
		t.Fatal(err)
	}
	for _, fi := range fis {
		if fi.Mode() != want[fi.Name()] {
			t.Errorf("readdir %s: mode %v, want %v", fi.Name(), fi.Mode(), want[fi.Name()])
		}
	}
	// a file that becomes a binary after it was first seen
	writeFile(t, c, "/empty", "\x7fELF")
	if fi, err := c.Stat("/empty"); err != nil || fi.Mode() != 0o755 {
		t.Errorf("rewritten file: %v, %v", fi.Mode(), err)
	}
}
