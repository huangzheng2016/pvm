package cli

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClipFileName(t *testing.T) {
	ok := map[string]string{
		"a.txt":         "a.txt",
		"./dir/b c.txt": "dir/b c.txt",
		"dir/":          "dir",
		"中文/文件.md":      "中文/文件.md",
		"dir/./x":       "dir/x",
	}
	for in, want := range ok {
		if got := clipFileName(in); got != want {
			t.Errorf("clipFileName(%q) = %q, want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", ".", "..", "../x", "a/../../x", "/etc/passwd", `a\b`, "c:x", "a:stream",
		"CON", "con.txt", "dir/NUL", "LPT1.log", "x?", "a|b", "trailing.", "trailing ", "bad\x01name"} {
		if got := clipFileName(bad); got != "" {
			t.Errorf("clipFileName(%q) = %q, want it refused", bad, got)
		}
	}
}

// Files copied in one direction come out the same in the other; links,
// bad names and paths leaving the folder do not.
func TestClipFilesRoundTrip(t *testing.T) {
	src := t.TempDir()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(src, "proj", "sub", "empty"), 0o755))
	must(os.WriteFile(filepath.Join(src, "proj", "sub", "文件.txt"), []byte("hello"), 0o644))
	must(os.WriteFile(filepath.Join(src, "one.bin"), bytes.Repeat([]byte{7}, 70000), 0o644))
	var buf bytes.Buffer
	must(checkClipFiles([]string{filepath.Join(src, "proj"), filepath.Join(src, "one.bin")}))
	must(writeClipFiles(&buf, []string{filepath.Join(src, "proj"), filepath.Join(src, "one.bin")}))
	// what a hostile VM could add
	tw := tar.NewWriter(&buf)
	for _, h := range []*tar.Header{
		{Name: "../escape.txt", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"},
		{Name: "CON", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644},
	} {
		must(tw.WriteHeader(h))
		if h.Size > 0 {
			tw.Write([]byte("x"))
		}
	}
	tw.Close()
	// the tar writer of the first part ends the archive; join the two
	data := buf.Bytes()
	first := bytes.Index(data, make([]byte, 1024))
	joined := append(append([]byte{}, data[:first]...), data[first+1024:]...)

	root := t.TempDir()
	dst := filepath.Join(root, "out")
	must(os.Mkdir(dst, 0o700))
	var skipped strings.Builder
	tops, err := extractClipFiles(bytes.NewReader(joined), dst, &skipped)
	must(err)
	want := []string{filepath.Join(dst, "proj"), filepath.Join(dst, "one.bin")}
	if strings.Join(tops, "|") != strings.Join(want, "|") {
		t.Errorf("tops = %q, want %q", tops, want)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "proj", "sub", "文件.txt")); err != nil || string(b) != "hello" {
		t.Errorf("nested file: %q, %v", b, err)
	}
	if fi, err := os.Stat(filepath.Join(dst, "proj", "sub", "empty")); err != nil || !fi.IsDir() {
		t.Errorf("empty folder lost: %v", err)
	}
	if fi, err := os.Stat(filepath.Join(dst, "one.bin")); err != nil || fi.Size() != 70000 {
		t.Errorf("one.bin: %v", err)
	}
	for _, p := range []string{filepath.Join(root, "escape.txt"), filepath.Join(dst, "link"), filepath.Join(dst, "CON")} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was written", p)
		}
	}
	if !strings.Contains(skipped.String(), "escape.txt") || !strings.Contains(skipped.String(), `"CON"`) {
		t.Errorf("skipped names not reported: %s", skipped.String())
	}
}

func TestExtractClipFilesLimit(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "big", Typeflag: tar.TypeReg, Size: maxClipFiles + 1, Mode: 0o644})
	if _, err := extractClipFiles(io.MultiReader(&buf, zeroReader{}), t.TempDir(), io.Discard); err == nil || !strings.Contains(err.Error(), "larger") {
		t.Fatalf("oversized copy accepted: %v", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}
