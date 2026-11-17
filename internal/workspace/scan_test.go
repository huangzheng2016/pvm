package workspace

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func canSymlink(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Symlink("x", filepath.Join(dir, "probe")); err != nil {
		t.Skipf("cannot create symlinks: %v", err)
	}
}

func TestScanHost(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.txt", "hello")
	write(t, root, "dir/b.txt", "world")
	write(t, root, "skip.log", "x")
	write(t, root, "node_modules/x.js", "x")
	m, err := NewMatcher(map[string][]byte{".gitignore": []byte("*.log\n")})
	if err != nil {
		t.Fatal(err)
	}
	got, skipped, err := ScanHost(root, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v", skipped)
	}
	var paths []string
	for _, e := range got.Sorted() {
		paths = append(paths, string(e.Type)+":"+e.Path)
	}
	if want := "file:a.txt dir:dir file:dir/b.txt"; strings.Join(paths, " ") != want {
		t.Fatalf("entries = %v, want %s", paths, want)
	}
	a := got["a.txt"]
	if a.Size != 5 || a.SHA256 != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Fatalf("a.txt = %+v", a)
	}
	if files, bytes := Totals(got); files != 2 || bytes != 10 {
		t.Fatalf("Totals = %d, %d", files, bytes)
	}

	// Same size and mtime: the previous hash is trusted without rereading.
	prev := Manifest{"a.txt": a}
	fake := prev["a.txt"]
	fake.SHA256 = "cached"
	fake.Exec = true
	prev["a.txt"] = fake
	again, _, err := ScanHost(root, m, prev)
	if err != nil {
		t.Fatal(err)
	}
	if again["a.txt"].SHA256 != "cached" {
		t.Fatalf("hash not reused: %+v", again["a.txt"])
	}
	if runtime.GOOS == "windows" && !again["a.txt"].Exec {
		t.Fatal("exec bit not carried over on Windows")
	}

	// A changed mtime forces a rehash.
	later := time.Unix(0, a.MTime).Add(time.Hour)
	if err := os.Chtimes(filepath.Join(root, "a.txt"), later, later); err != nil {
		t.Fatal(err)
	}
	again, _, err = ScanHost(root, m, prev)
	if err != nil {
		t.Fatal(err)
	}
	if again["a.txt"].SHA256 != a.SHA256 {
		t.Fatalf("not rehashed: %+v", again["a.txt"])
	}
	// Windows cannot express the exec bit, so even a content edit keeps it.
	write(t, root, "a.txt", "edited")
	again, _, err = ScanHost(root, m, prev)
	if err != nil {
		t.Fatal(err)
	}
	if again["a.txt"].SHA256 == a.SHA256 || again["a.txt"].Exec != (runtime.GOOS == "windows") {
		t.Fatalf("after edit: %+v", again["a.txt"])
	}
}

func TestScanHostExecAndSymlink(t *testing.T) {
	root := t.TempDir()
	write(t, root, "run.sh", "#!/bin/sh\n")
	if runtime.GOOS != "windows" {
		if err := os.Chmod(filepath.Join(root, "run.sh"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	canSymlink(t)
	if err := os.Symlink(filepath.Join("..", "outside"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	got, _, err := ScanHost(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if l := got["link"]; l.Type != TypeSymlink || l.Link != "../outside" {
		t.Fatalf("link = %+v", l)
	}
	if runtime.GOOS != "windows" && !got["run.sh"].Exec {
		t.Fatal("exec bit not recorded")
	}
}

func TestScanHostSkipsInvalidUTF8(t *testing.T) {
	root := t.TempDir()
	// Only Linux file systems accept arbitrary bytes in names.
	if err := os.WriteFile(filepath.Join(root, "caf\xe9.txt"), nil, 0o644); err != nil {
		t.Skipf("cannot create a non-UTF-8 name: %v", err)
	}
	if ents, _ := os.ReadDir(root); len(ents) != 1 || ents[0].Name() != "caf\xe9.txt" {
		t.Skip("the file system rewrote the non-UTF-8 name")
	}
	got, skipped, err := ScanHost(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || len(skipped) != 1 || skipped[0].Path != "caf\xe9.txt" {
		t.Fatalf("got %v, skipped %v", got, skipped)
	}
}
