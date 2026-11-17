package sftpd

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestServeJunction(t *testing.T) {
	c, root, outside := fixture(t, false)
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(root, "junc"), outside).CombinedOutput(); err != nil {
		t.Skipf("mklink /J: %v: %s", err, out)
	}
	if got, err := readFile(c, "/junc/secret.txt"); err == nil || got == "secret" {
		t.Errorf("read through junction = %q, %v; want error", got, err)
	}
	if fis, err := c.ReadDir("/junc"); err == nil {
		t.Errorf("listed through junction: %v", fis)
	}
	if _, err := c.Create("/junc/new.txt"); err == nil {
		t.Error("created a file through the junction")
	}
	if _, err := os.Stat(filepath.Join(outside, "new.txt")); err == nil {
		t.Error("file written outside root")
	}
	if target, err := c.ReadLink("/junc"); err == nil {
		t.Errorf("ReadLink revealed junction target %q", target)
	}
}

func shortName(t *testing.T, p string) string {
	t.Helper()
	long, err := windows.UTF16PtrFromString(p)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]uint16, windows.MAX_PATH)
	n, err := windows.GetShortPathName(long, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 {
		t.Skipf("GetShortPathName: %v", err)
	}
	short := filepath.Base(windows.UTF16ToString(buf[:n]))
	if short == filepath.Base(p) {
		t.Skip("8.3 names are disabled on this volume")
	}
	return short
}

func TestServeShortNames(t *testing.T) {
	c, root, outside := fixture(t, false)
	longOutside := filepath.Join(filepath.Dir(outside), "outside-long-directory-name")
	if err := os.Rename(outside, longOutside); err != nil {
		t.Fatal(err)
	}
	short := shortName(t, longOutside)
	for _, p := range []string{"/" + short + "/secret.txt", "/../" + short + "/secret.txt"} {
		if got, err := readFile(c, p); err == nil || got == "secret" {
			t.Errorf("read %q = %q, %v; want error", p, got, err)
		}
	}
	inside := filepath.Join(root, "inside-long-directory-name")
	if err := os.Mkdir(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inside, "f.txt"), []byte("in"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := readFile(c, "/"+shortName(t, inside)+"/f.txt"); err != nil || got != "in" {
		t.Errorf("read via inside short name = %q, %v", got, err)
	}
}

func TestServeNameCase(t *testing.T) {
	c, root, _ := fixture(t, false)
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "file.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := readFile(c, "/SUB/FILE.TXT"); err != nil || got != "x" {
		t.Errorf("case-insensitive read = %q, %v", got, err)
	}
	// The root folder's own name, in another case, resolves inside root.
	if _, err := readFile(c, "/../ROOT/sub/file.txt"); err == nil {
		t.Error("ROOT/sub/file.txt resolved outside the root")
	}
}
