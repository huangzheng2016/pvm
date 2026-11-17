package workspace

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

type tarEntry struct {
	name, link, body string
	typ              byte
	mode             int64
}

func buildTar(t *testing.T, entries ...tarEntry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		mode := e.mode
		if mode == 0 {
			mode = 0o644
		}
		hdr := &tar.Header{Name: e.name, Typeflag: typ, Linkname: e.link, Mode: mode, Size: int64(len(e.body)), ModTime: time.Unix(1700000000, 0)}
		if typ != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Size > 0 {
			tw.Write([]byte(e.body))
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

func openRoot(t *testing.T, dir string) *os.Root {
	t.Helper()
	r, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestTarRoundTrip(t *testing.T) {
	src := t.TempDir()
	write(t, src, "a.txt", "alpha")
	write(t, src, "dir/run.sh", "#!/bin/sh\n")
	write(t, src, "dir/empty/.keep", "")
	if runtime.GOOS != "windows" {
		os.Chmod(filepath.Join(src, "dir", "run.sh"), 0o755)
	}
	m, _, err := ScanHost(src, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		e := m["dir/run.sh"]
		e.Exec = true
		m["dir/run.sh"] = e
	}
	var buf bytes.Buffer
	if err := WriteTar(&buf, src, m.Sorted(), "ws/"); err != nil {
		t.Fatal(err)
	}

	// The archive layout the guest receives.
	tr := tar.NewReader(bytes.NewReader(buf.Bytes()))
	var names []string
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, hdr.Name)
		if hdr.Name == "ws/dir/run.sh" && hdr.Mode != 0o755 {
			t.Errorf("run.sh mode = %o", hdr.Mode)
		}
		if hdr.Uid != 0 || hdr.Uname != "" {
			t.Errorf("%s owner = %d %q", hdr.Name, hdr.Uid, hdr.Uname)
		}
	}
	if want := []string{"ws/a.txt", "ws/dir/", "ws/dir/empty/", "ws/dir/empty/.keep", "ws/dir/run.sh"}; !slices.Equal(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}

	dst := t.TempDir()
	opts := DefaultExtractOptions()
	opts.StripPrefix = "ws/"
	opts.Expect = m
	res, err := ExtractTar(&buf, openRoot(t, dst), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rejected) != 0 {
		t.Fatalf("rejected = %v", res.Rejected)
	}
	got, _, err := ScanHost(dst, nil, m)
	if err != nil {
		t.Fatal(err)
	}
	for p, e := range m {
		g := got[p]
		if !g.SameContent(e) {
			t.Errorf("%s: got %+v, want %+v", p, g, e)
		}
		if e.Type == TypeFile && g.MTime != e.MTime {
			t.Errorf("%s: mtime %d, want %d", p, g.MTime, e.MTime)
		}
	}
}

func TestWriteTarDetectsChange(t *testing.T) {
	src := t.TempDir()
	write(t, src, "a.txt", "alpha")
	m, _, err := ScanHost(src, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	write(t, src, "a.txt", "omega") // same size, different content
	err = WriteTar(&bytes.Buffer{}, src, m.Sorted(), "")
	if err == nil || !strings.Contains(err.Error(), "a.txt changed during transfer") {
		t.Fatalf("err = %v", err)
	}
	write(t, src, "a.txt", "longer")
	err = WriteTar(&bytes.Buffer{}, src, m.Sorted(), "")
	if err == nil || !strings.Contains(err.Error(), "changed during transfer") {
		t.Fatalf("err = %v", err)
	}
}

func TestExtractRejects(t *testing.T) {
	cases := []struct {
		e       tarEntry
		windows bool
		reason  string
	}{
		{tarEntry{name: "/etc/passwd"}, false, "absolute"},
		{tarEntry{name: "../x"}, false, ".."},
		{tarEntry{name: "a/../../x"}, false, ".."},
		{tarEntry{name: "a//x"}, false, "empty"},
		{tarEntry{name: "./x"}, false, "'.'"},
		{tarEntry{name: `a\x`}, false, "backslash"},
		{tarEntry{name: "dev", typ: tar.TypeChar}, false, "unsupported entry type"},
		{tarEntry{name: "fifo", typ: tar.TypeFifo}, false, "unsupported entry type"},
		{tarEntry{name: "hard", typ: tar.TypeLink, link: "ok"}, false, "unsupported entry type"},
		{tarEntry{name: "abs", typ: tar.TypeSymlink, link: "/etc"}, false, "absolute"},
		{tarEntry{name: "win", typ: tar.TypeSymlink, link: `C:\x`}, false, "absolute"},
		{tarEntry{name: "a/up", typ: tar.TypeSymlink, link: "../../x"}, false, "outside"},
		{tarEntry{name: "up", typ: tar.TypeSymlink, link: ".."}, false, "outside"},
		{tarEntry{name: "via", typ: tar.TypeSymlink, link: "d/../x"}, false, "after a name"},
		{tarEntry{name: "a:b"}, true, "not allowed on Windows"},
		{tarEntry{name: "q?"}, true, "not allowed on Windows"},
		{tarEntry{name: "ctl\x01"}, true, "not allowed on Windows"},
		{tarEntry{name: "dot."}, true, "dot or space"},
		{tarEntry{name: "space "}, true, "dot or space"},
		{tarEntry{name: "CON"}, true, "reserved"},
		{tarEntry{name: "d/aux.c"}, true, "reserved"},
		{tarEntry{name: "com1.txt"}, true, "reserved"},
		{tarEntry{name: "Lpt9"}, true, "reserved"},
	}
	for _, c := range cases {
		dst := t.TempDir()
		opts := ExtractOptions{WindowsNames: c.windows}
		res, err := ExtractTar(buildTar(t, c.e, tarEntry{name: "ok", body: "ok"}), openRoot(t, dst), opts)
		if err != nil {
			t.Errorf("%q: %v", c.e.name, err)
			continue
		}
		if len(res.Rejected) != 1 || res.Rejected[0].Path != c.e.name || !strings.Contains(res.Rejected[0].Reason, c.reason) {
			t.Errorf("%q: rejected = %v, want reason %q", c.e.name, res.Rejected, c.reason)
		}
		if !slices.Equal(res.Written, []string{"ok"}) {
			t.Errorf("%q: written = %v", c.e.name, res.Written)
		}
		ents, _ := os.ReadDir(dst)
		if len(ents) != 1 {
			t.Errorf("%q: destination has %d entries", c.e.name, len(ents))
		}
	}
	// Names Windows rejects are fine elsewhere.
	if runtime.GOOS != "windows" {
		res, err := ExtractTar(buildTar(t, tarEntry{name: "aux.c", body: "x"}), openRoot(t, t.TempDir()), ExtractOptions{})
		if err != nil || len(res.Rejected) != 0 {
			t.Fatalf("aux.c without WindowsNames: %v %v", res, err)
		}
	}
}

func TestExtractCaseAndDuplicates(t *testing.T) {
	dst := t.TempDir()
	tarball := buildTar(t,
		tarEntry{name: "Dir/a", body: "1"},
		tarEntry{name: "dir/b", body: "2"},
		tarEntry{name: "README", body: "3"},
		tarEntry{name: "readme", body: "4"},
		tarEntry{name: "x", body: "5"},
		tarEntry{name: "x", body: "6"},
	)
	res, err := ExtractTar(tarball, openRoot(t, dst), ExtractOptions{CaseInsensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	var rejected []string
	for _, r := range res.Rejected {
		rejected = append(rejected, r.Path)
	}
	if !slices.Equal(rejected, []string{"dir/b", "readme", "x"}) {
		t.Fatalf("rejected = %v", res.Rejected)
	}
	if !slices.Equal(res.Written, []string{"Dir/a", "README", "x"}) {
		t.Fatalf("written = %v", res.Written)
	}
}

func TestExtractSymlinkInside(t *testing.T) {
	dst := t.TempDir()
	res, err := ExtractTar(buildTar(t,
		tarEntry{name: "a/b", body: "x"},
		tarEntry{name: "a/link", typ: tar.TypeSymlink, link: "../a/b"},
	), openRoot(t, dst), ExtractOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rejected) != 0 {
		t.Fatalf("rejected = %v", res.Rejected)
	}
	if len(res.SkippedSymlinks) == 1 {
		t.Skip("host cannot create symlinks")
	}
	if l, err := os.Readlink(filepath.Join(dst, "a", "link")); err != nil || filepath.ToSlash(l) != "../a/b" {
		t.Fatalf("link = %q, %v", l, err)
	}
}

func TestExtractExclusiveAndOverwrite(t *testing.T) {
	dst := t.TempDir()
	write(t, dst, "a", "old")
	if err := os.Mkdir(filepath.Join(dst, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	root := openRoot(t, dst)

	_, err := ExtractTar(buildTar(t, tarEntry{name: "a", body: "new"}), root, ExtractOptions{})
	if err == nil {
		t.Fatal("exclusive extraction overwrote an existing file")
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a")); string(b) != "old" {
		t.Fatalf("a = %q", b)
	}

	res, err := ExtractTar(buildTar(t,
		tarEntry{name: "a", body: "new", mode: 0o755},
		tarEntry{name: "d", body: "now a file"},
	), root, ExtractOptions{Overwrite: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Written, []string{"a", "d"}) {
		t.Fatalf("written = %v", res.Written)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a")); string(b) != "new" {
		t.Fatalf("a = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "d")); string(b) != "now a file" {
		t.Fatalf("d = %q", b)
	}
	if fi, err := os.Stat(filepath.Join(dst, "a")); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o755 {
		t.Fatalf("a mode = %v", fi.Mode())
	}
	ents, _ := os.ReadDir(dst)
	if len(ents) != 2 {
		t.Fatalf("leftover temporary files: %v", ents)
	}

	// A directory that still holds files, such as ignored ones the plan
	// never saw, is not replaced, whatever type the new entry has.
	write(t, dst, "ignored/keep", "k")
	for _, e := range []tarEntry{{name: "ignored", body: "file"}, {name: "ignored", typ: tar.TypeSymlink, link: "a"}} {
		_, err := ExtractTar(buildTar(t, e), root, ExtractOptions{Overwrite: true})
		if err == nil || !strings.Contains(err.Error(), "ignored") {
			t.Errorf("type %c over a non-empty directory: %v", e.typ, err)
		}
		if b, _ := os.ReadFile(filepath.Join(dst, "ignored", "keep")); string(b) != "k" {
			t.Fatalf("ignored/keep = %q", b)
		}
	}
	if ents, _ := os.ReadDir(dst); len(ents) != 3 {
		t.Fatalf("leftover temporary files: %v", ents)
	}
}

func TestExtractChecksExistingPaths(t *testing.T) {
	dst := t.TempDir()
	write(t, dst, "Makefile", "host")
	write(t, dst, "Src/a", "host")
	root := openRoot(t, dst)
	res, err := ExtractTar(buildTar(t,
		tarEntry{name: "makefile", body: "vm"},
		tarEntry{name: "src/b", body: "vm"},
		tarEntry{name: "Src/c", body: "vm"},
	), root, ExtractOptions{Overwrite: true, CaseInsensitive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rejected) != 2 || !strings.Contains(res.Rejected[0].Reason, "Makefile") || !strings.Contains(res.Rejected[1].Reason, "Src") {
		t.Fatalf("rejected = %v", res.Rejected)
	}
	if !slices.Equal(res.Written, []string{"Src/c"}) {
		t.Fatalf("written = %v", res.Written)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "Makefile")); string(b) != "host" {
		t.Fatalf("Makefile = %q", b)
	}

	// A symlinked parent is never written through, whether it was in the
	// destination before or arrives in the same archive.
	canSymlink(t)
	if err := os.Symlink(".", filepath.Join(dst, "loop")); err != nil {
		t.Fatal(err)
	}
	res, err = ExtractTar(buildTar(t,
		tarEntry{name: "loop/x", body: "x"},
		tarEntry{name: "d", typ: tar.TypeSymlink, link: "."},
		tarEntry{name: "d/l", typ: tar.TypeSymlink, link: "../outside"},
	), root, ExtractOptions{Overwrite: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rejected) != 2 || res.Rejected[0].Path != "loop/x" || res.Rejected[1].Path != "d/l" {
		t.Fatalf("rejected = %v", res.Rejected)
	}
	for _, p := range []string{"x", "l"} {
		if _, err := os.Lstat(filepath.Join(dst, p)); !os.IsNotExist(err) {
			t.Errorf("%s written through a symlink: %v", p, err)
		}
	}
}

func TestExtractOnlyAndExpect(t *testing.T) {
	dst := t.TempDir()
	opts := ExtractOptions{
		Only:   map[string]bool{"a": true, "b": true},
		Expect: Manifest{"a": {Path: "a", Type: TypeFile, SHA256: sum("A")}, "b": {Path: "b", Type: TypeFile, SHA256: sum("B")}},
	}
	res, err := ExtractTar(buildTar(t,
		tarEntry{name: "a", body: "A"},
		tarEntry{name: "b", body: "changed"},
		tarEntry{name: "c", body: "C"},
	), openRoot(t, dst), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.Written, []string{"a"}) {
		t.Fatalf("written = %v", res.Written)
	}
	if len(res.Rejected) != 2 || res.Rejected[0].Path != "b" || res.Rejected[1].Path != "c" {
		t.Fatalf("rejected = %v", res.Rejected)
	}
	for _, p := range []string{"b", "c"} {
		if _, err := os.Lstat(filepath.Join(dst, p)); !os.IsNotExist(err) {
			t.Errorf("%s written: %v", p, err)
		}
	}

	opts = ExtractOptions{Expect: Manifest{"l": {Path: "l", Type: TypeSymlink, Link: "a"}, "d": {Path: "d", Type: TypeFile}}}
	res, err = ExtractTar(buildTar(t,
		tarEntry{name: "l", typ: tar.TypeSymlink, link: "b"},
		tarEntry{name: "d", typ: tar.TypeDir},
	), openRoot(t, t.TempDir()), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rejected) != 2 || len(res.Written) != 0 {
		t.Fatalf("changed symlink and type accepted: %+v", res)
	}
}

func TestRemoveFromRoot(t *testing.T) {
	dst := t.TempDir()
	write(t, dst, "d/f", "x")
	root := openRoot(t, dst)
	if err := RemoveFromRoot(root, "d"); err == nil {
		t.Fatal("removed a non-empty directory")
	}
	for _, p := range []string{"d/f", "d", "missing"} {
		if err := RemoveFromRoot(root, p); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	if err := RemoveFromRoot(root, "../x"); err == nil {
		t.Fatal("accepted ..")
	}
	if ents, _ := os.ReadDir(dst); len(ents) != 0 {
		t.Fatalf("left %v", ents)
	}
}

func TestHostExecutable(t *testing.T) {
	got := HostExecutable([]string{
		".git/hooks/pre-commit", ".git/config", ".vscode/tasks.json", ".envrc", "sub/.envrc",
		"src/main.go", ".git/hooks/sub/x", "sub/.vscode/tasks.json", ".git/HEAD",
	})
	want := []string{".git/hooks/pre-commit", ".git/config", ".vscode/tasks.json", ".envrc", "sub/.envrc"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
