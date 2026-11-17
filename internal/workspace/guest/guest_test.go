package guest

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/workspace"
)

var badPaths = []string{
	"", "/workspace", "/workspace/", "/workspace/../etc", "/workspace/Foo",
	"/workspace/a/b", "/tmp/a", "workspace/a", "/workspace/-a", "/workspace/a'b", "/workspace/a\n",
}

func TestBuildersRejectInvalidPaths(t *testing.T) {
	builders := map[string]func(string) (string, error){
		"prepare":  PrepareCommand,
		"state":    ExistsNonEmptyCommand,
		"list":     ListCommand,
		"hash":     HashCommand,
		"fresh":    func(p string) (string, error) { return UploadCommand(p, true) },
		"push":     func(p string) (string, error) { return UploadCommand(p, false) },
		"rename":   RenameCommand,
		"download": DownloadCommand,
		"remove":   RemoveCommand,
		"mounted":  MountedCommand,
	}
	for name, b := range builders {
		for _, p := range badPaths {
			if cmd, err := b(p); err == nil {
				t.Errorf("%s(%q) = %q, want error", name, p, cmd)
			}
		}
		cmd, err := b("/workspace/hello-world")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.HasPrefix(cmd, "bash -c '") || !strings.Contains(cmd, " pvm '/workspace/hello-world'") {
			t.Errorf("%s: unexpected command %q", name, cmd)
		}
	}
}

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		"":          "''",
		"a b":       "'a b'",
		"it's":      `'it'\''s'`,
		"$(x)\"\\`": "'$(x)\"\\`'",
		"''":        `''\'''\'''`,
	} {
		if got := quote(in); got != want {
			t.Errorf("quote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestCommandPassesArgsPositionally(t *testing.T) {
	cmd := command(`echo "$1"`+"\n", "it's")
	if !strings.HasSuffix(cmd, ` pvm 'it'\''s'`) {
		t.Fatalf("command = %q", cmd)
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	out, err := exec.Command("bash", "-c", cmd).Output()
	if err != nil || string(out) != "it's\n" {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestParseList(t *testing.T) {
	rec := func(f ...string) string { return strings.Join(f, "\x00") + "\x00" }
	out := rec("d", "4096", "1700000000.5000000000", "755", "", "src") +
		rec("f", "12", "1700000001.1234567890", "644", "", "src/a b.txt") +
		rec("f", "3", "1700000002", "750", "", "run.sh") +
		rec("l", "5", "1700000003.0", "777", "../up", "src/link") +
		rec("p", "0", "1700000004.0", "644", "", "fifo") +
		rec("s", "0", "1700000004.0", "755", "", "sock") +
		rec("f", "0", "1.0", "600", "", "line\nbreak") +
		rec("f", "0", "1.0", "644", "", "caf\xe9.txt")
	m, skipped, err := ParseList([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	want := workspace.Manifest{
		"src":         {Path: "src", Type: workspace.TypeDir},
		"src/a b.txt": {Path: "src/a b.txt", Type: workspace.TypeFile, Size: 12, MTime: 1700000001123456789},
		"run.sh":      {Path: "run.sh", Type: workspace.TypeFile, Size: 3, MTime: 1700000002000000000, Exec: true},
		"src/link":    {Path: "src/link", Type: workspace.TypeSymlink, Link: "../up"},
		"line\nbreak": {Path: "line\nbreak", Type: workspace.TypeFile, MTime: 1000000000},
	}
	if len(m) != len(want) {
		t.Fatalf("got %d entries: %#v", len(m), m)
	}
	for k, w := range want {
		if m[k] != w {
			t.Errorf("%q = %#v, want %#v", k, m[k], w)
		}
	}
	wantSkipped := []Skipped{{"fifo", "FIFO"}, {"sock", "socket"}, {"caf\xe9.txt", "name is not valid UTF-8"}}
	if !slices.Equal(skipped, wantSkipped) {
		t.Errorf("skipped = %v, want %v", skipped, wantSkipped)
	}

	if m, _, err := ParseList(nil); err != nil || len(m) != 0 {
		t.Errorf("empty list: %v %v", m, err)
	}
	for _, bad := range []string{
		"f\x00", // truncated
		rec("f", "1", "1.0", "644", "", "a") + "x",
		rec("f", "x", "1.0", "644", "", "a"),
		rec("f", "1", "abc", "644", "", "a"),
		rec("f", "1", "1.0", "9z", "", "a"),
		rec("f", "1", "1.0", "644", "", ""),
		rec("f", "1", "1.0", "644", "", "/etc/passwd"),
		rec("f", "1", "1.0", "644", "", "a/../b"),
		rec("f", "1", "1.0", "644", "", "./a"),
		rec("f", "1", "1.0", "644", "", "a//b"),
		rec("f", "1", "1.0", "644", "", "a") + rec("d", "1", "1.0", "755", "", "a"),
	} {
		if _, _, err := ParseList([]byte(bad)); err == nil {
			t.Errorf("ParseList(%q) succeeded", bad)
		}
	}
}

func TestParseMTime(t *testing.T) {
	for in, want := range map[string]int64{
		"0":                     0,
		"1700000000.1234567890": 1700000000123456789,
		"1.5":                   1500000000,
		"-1.5":                  -1500000000,
	} {
		if got, err := parseMTime(in); err != nil || got != want {
			t.Errorf("parseMTime(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
}

func TestParseHashes(t *testing.T) {
	h1 := strings.Repeat("a", 64)
	h2 := strings.Repeat("0", 63) + "f"
	out := h1 + "  src/a b.txt\x00" + h2 + " *new\nline\x00" + h1 + "  back\\slash\x00"
	got, err := ParseHashes([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"src/a b.txt": h1, "new\nline": h2, "back\\slash": h1}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%q = %q, want %q", k, got[k], v)
		}
	}
	if got, err := ParseHashes(nil); err != nil || len(got) != 0 {
		t.Errorf("empty: %v %v", got, err)
	}
	for _, bad := range []string{
		h1 + "  a",                           // missing terminator
		h1 + "  \x00",                        // empty name
		strings.Repeat("A", 64) + "  a\x00",  // upper case
		strings.Repeat("a", 63) + "   a\x00", // short sum
		h1 + "-+a\x00",                       // bad separator
	} {
		if _, err := ParseHashes([]byte(bad)); err == nil {
			t.Errorf("ParseHashes(%q) succeeded", bad)
		}
	}
}

func TestParseStateFreeMounted(t *testing.T) {
	if s, err := ParseState([]byte("nonempty\n")); err != nil || s != StateNonEmpty {
		t.Errorf("ParseState = %v %v", s, err)
	}
	if _, err := ParseState([]byte("maybe\n")); err == nil {
		t.Error("ParseState accepted junk")
	}
	if n, err := ParseFreeSpace([]byte("21474836480\n")); err != nil || n != 21474836480 {
		t.Errorf("ParseFreeSpace = %d %v", n, err)
	}
	for _, bad := range []string{"", "Avail\n", "-1", "12 34"} {
		if _, err := ParseFreeSpace([]byte(bad)); err == nil {
			t.Errorf("ParseFreeSpace(%q) succeeded", bad)
		}
	}
	if m, err := ParseMounted([]byte("yes\n")); err != nil || !m {
		t.Errorf("ParseMounted yes = %v %v", m, err)
	}
	if m, err := ParseMounted([]byte("no\n")); err != nil || m {
		t.Errorf("ParseMounted no = %v %v", m, err)
	}
	if _, err := ParseMounted(nil); err == nil {
		t.Error("ParseMounted accepted empty output")
	}
}

// The integration test runs the real scripts against a temp directory with
// the local bash and GNU tools, as ssh would hand them to the login shell.
func requireTools(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	probe := `set -e; for c in find tar sha256sum xargs mktemp; do command -v $c >/dev/null; done
find . -maxdepth 0 -printf '' && tar --version | grep -q GNU && sha256sum -z --version >/dev/null`
	if err := exec.Command("bash", "-c", probe).Run(); err != nil {
		t.Skip("GNU find, tar or coreutils not available")
	}
}

func run(t *testing.T, cmd string, stdin []byte) ([]byte, error) {
	t.Helper()
	c := exec.Command("bash", "-c", cmd)
	c.Stdin = bytes.NewReader(stdin)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		t.Logf("stderr: %s", stderr.String())
	}
	return out, err
}

type tarEnt struct {
	name, body string
	mode       int64
	dir        bool
}

func mkTar(t *testing.T, ents ...tarEnt) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range ents {
		h := &tar.Header{Name: e.name, Mode: e.mode, Size: int64(len(e.body)), Typeflag: tar.TypeReg, ModTime: time.Unix(1700000000, 0)}
		if e.dir {
			h.Typeflag, h.Size = tar.TypeDir, 0
		}
		if h.Mode == 0 {
			h.Mode = 0o644
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestScriptsIntegration(t *testing.T) {
	requireTools(t)
	root := filepath.ToSlash(t.TempDir())
	ws, st := root+"/ws", root+"/st"
	gp := "/workspace/demo"
	// Run the validated builders, then point them at the temp directory.
	local := func(cmd string, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return strings.ReplaceAll(cmd, quote(gp), quote(ws))
	}

	fresh := mkTar(t,
		tarEnt{name: "files/", dir: true, mode: 0o755},
		tarEnt{name: "files/a.txt", body: "alpha"},
		tarEnt{name: "files/it's dir/", dir: true, mode: 0o755},
		tarEnt{name: "files/it's dir/b.sh", body: "#!/bin/sh\n", mode: 0o755},
		tarEnt{name: "files/-dash", body: "dash"},
		tarEnt{name: "files/empty/", dir: true, mode: 0o755},
	)
	if _, err := run(t, uploadCmd(ws, st, true), fresh); err != nil {
		t.Fatalf("fresh upload: %v", err)
	}
	if got := readFile(t, ws+"/it's dir/b.sh"); got != "#!/bin/sh\n" {
		t.Fatalf("b.sh = %q", got)
	}
	if ents, _ := os.ReadDir(st); len(ents) != 0 {
		t.Errorf("staging not cleaned: %v", ents)
	}

	out, err := run(t, local(ExistsNonEmptyCommand(gp)), nil)
	if s, perr := ParseState(out); err != nil || perr != nil || s != StateNonEmpty {
		t.Errorf("state = %q %v %v", out, err, perr)
	}

	// A second fresh upload must refuse the non-empty directory untouched.
	if _, err := run(t, uploadCmd(ws, st, true), mkTar(t, tarEnt{name: "files/x", body: "x"})); err == nil {
		t.Fatal("fresh upload over a non-empty directory succeeded")
	}
	if _, err := os.Stat(ws + "/x"); err == nil || readFile(t, ws+"/a.txt") != "alpha" {
		t.Fatal("refused upload changed the workspace")
	}

	out, err = run(t, local(ListCommand(gp)), nil)
	if err != nil {
		t.Fatal(err)
	}
	m, skipped, err := ParseList(out)
	if err != nil || len(skipped) != 0 {
		t.Fatalf("ParseList: %v %v", err, skipped)
	}
	for p, typ := range map[string]workspace.EntryType{
		"a.txt": workspace.TypeFile, "it's dir": workspace.TypeDir, "it's dir/b.sh": workspace.TypeFile,
		"-dash": workspace.TypeFile, "empty": workspace.TypeDir,
	} {
		if m[p].Type != typ {
			t.Errorf("%q type = %q, want %q", p, m[p].Type, typ)
		}
	}
	if len(m) != 5 || m["a.txt"].Size != 5 || m["a.txt"].MTime != 1700000000e9 {
		t.Errorf("manifest = %#v", m)
	}
	if runtime.GOOS != "windows" && (!m["it's dir/b.sh"].Exec || m["a.txt"].Exec) {
		t.Errorf("exec bits wrong: %#v", m)
	}

	out, err = run(t, local(HashCommand(gp)), []byte("a.txt\x00-dash\x00it's dir/b.sh\x00"))
	if err != nil {
		t.Fatal(err)
	}
	sums, err := ParseHashes(out)
	if err != nil {
		t.Fatal(err)
	}
	for p, body := range map[string]string{"a.txt": "alpha", "-dash": "dash", "it's dir/b.sh": "#!/bin/sh\n"} {
		s := sha256.Sum256([]byte(body))
		if sums[p] != hex.EncodeToString(s[:]) {
			t.Errorf("hash %q = %q", p, sums[p])
		}
	}
	if out, err := run(t, local(HashCommand(gp)), nil); err != nil || len(out) != 0 {
		t.Errorf("empty hash = %q %v", out, err)
	}

	// Push: delete a file and an empty dir, change a file into a dir, add new paths.
	push := mkTar(t,
		tarEnt{name: "files/a.txt/", dir: true, mode: 0o755},
		tarEnt{name: "files/a.txt/inner", body: "inner"},
		tarEnt{name: "files/it's dir/b.sh", body: "v2", mode: 0o755},
		tarEnt{name: "files/new/deep/c.txt", body: "c"},
		tarEnt{name: "pvm/deletes", body: "-dash\x00empty\x00"},
	)
	if _, err := run(t, uploadCmd(ws, st, false), push); err != nil {
		t.Fatalf("push: %v", err)
	}
	if readFile(t, ws+"/a.txt/inner") != "inner" || readFile(t, ws+"/it's dir/b.sh") != "v2" || readFile(t, ws+"/new/deep/c.txt") != "c" {
		t.Error("push did not apply new contents")
	}
	for _, p := range []string{"-dash", "empty"} {
		if _, err := os.Lstat(ws + "/" + p); err == nil {
			t.Errorf("%s not deleted", p)
		}
	}
	// A dir that is now a file again: the dir's child is deleted first.
	if _, err := run(t, uploadCmd(ws, st, false), mkTar(t,
		tarEnt{name: "files/a.txt", body: "alpha2"},
		tarEnt{name: "pvm/deletes", body: "a.txt/inner\x00"},
	)); err != nil || readFile(t, ws+"/a.txt") != "alpha2" {
		t.Fatalf("push dir->file: %v", err)
	}

	for _, bad := range []string{"../x\x00", "/etc/x\x00", "a/./b\x00", "\x00"} {
		if _, err := run(t, uploadCmd(ws, st, false), mkTar(t, tarEnt{name: "pvm/deletes", body: bad})); err == nil {
			t.Errorf("push with delete %q succeeded", bad)
		}
	}
	if _, err := run(t, uploadCmd(ws, st, false), mkTar(t, tarEnt{name: "pvm/deletes", body: "new\x00"})); err == nil {
		t.Error("deleting a non-empty directory succeeded")
	}
	if ents, _ := os.ReadDir(st); len(ents) != 0 {
		t.Errorf("staging not cleaned: %v", ents)
	}

	if _, err := run(t, local(RenameCommand(gp)), []byte("new/deep/c.txt\x00new/deep/c.txt.pvm-conflict-vm\x00")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if readFile(t, ws+"/new/deep/c.txt.pvm-conflict-vm") != "c" {
		t.Error("rename did not move the file")
	}
	if _, err := run(t, local(RenameCommand(gp)), []byte("a.txt\x00it's dir/b.sh\x00")); err == nil {
		t.Error("rename over an existing path succeeded")
	}

	out, err = run(t, local(DownloadCommand(gp)), []byte("a.txt\x00new\x00it's dir/b.sh\x00"))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(bytes.NewReader(out))
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, h.Name)
	}
	if want := []string{"a.txt", "new/", "it's dir/b.sh"}; !slices.Equal(names, want) {
		t.Errorf("download names = %q, want %q", names, want)
	}
	if out, err := run(t, local(DownloadCommand(gp)), nil); err != nil || len(out) == 0 {
		t.Errorf("empty download = %d bytes, %v", len(out), err)
	}

	// Point the mount check at a fake mountinfo; first with a mount below
	// the workspace, which must stop the delete.
	mountinfo := root + "/mountinfo"
	remove := strings.ReplaceAll(local(RemoveCommand(gp)), "/proc/self/mountinfo", mountinfo)
	if err := os.WriteFile(mountinfo, []byte("36 25 0:32 / "+ws+"/new rw - 9p host rw\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, remove, nil); err == nil {
		t.Fatal("remove with a mount below the workspace succeeded")
	}
	if readFile(t, ws+"/a.txt") != "alpha2" {
		t.Fatal("remove deleted files despite the mount")
	}
	if err := os.WriteFile(mountinfo, []byte("22 1 8:1 / / rw - ext4 /dev/vda1 rw\n36 25 0:32 / "+ws+"x rw - 9p host rw\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, remove, nil); err != nil {
		t.Fatal(err)
	}
	out, err = run(t, local(ExistsNonEmptyCommand(gp)), nil)
	if s, _ := ParseState(out); err != nil || s != StateAbsent {
		t.Errorf("after remove: %q %v", out, err)
	}
}

func TestPushRejectsSymlinkedParent(t *testing.T) {
	requireTools(t)
	root := filepath.ToSlash(t.TempDir())
	ws, st, outside := root+"/ws", root+"/st", root+"/outside"
	for _, d := range []string{ws, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(outside+"/keep", []byte("k"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, ws+"/link"); err != nil {
		t.Skipf("cannot create symlinks: %v", err)
	}
	if out, err := exec.Command("bash", "-c", `[ -L "$1" ]`, "x", ws+"/link").CombinedOutput(); err != nil {
		t.Skipf("bash does not see the symlink: %s", out)
	}
	if _, err := run(t, uploadCmd(ws, st, false), mkTar(t, tarEnt{name: "pvm/deletes", body: "link/keep\x00"})); err == nil {
		t.Error("delete through a symlink succeeded")
	}
	// A staged directory replaces the symlink instead of writing through it.
	if _, err := run(t, uploadCmd(ws, st, false), mkTar(t, tarEnt{name: "files/link/new", body: "x"})); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(ws + "/link"); err != nil || !fi.IsDir() || readFile(t, ws+"/link/new") != "x" {
		t.Errorf("link was not replaced by a directory: %v", err)
	}
	if readFile(t, outside+"/keep") != "k" {
		t.Error("file outside the workspace changed")
	}
	if _, err := os.Stat(outside + "/new"); err == nil {
		t.Error("file written outside the workspace")
	}
}

func TestPrepareLinksHomeWorkspace(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the script runs in Linux guests and needs GNU stat")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	root, home := dir+"/root", dir+"/home"
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	prepare := func() string {
		t.Helper()
		c := exec.Command("bash", "-c", command(prepareScript, root+"/web", root, root+"/.pvm-staging"))
		c.Env = append(os.Environ(), "HOME="+home)
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("prepare: %v\n%s", err, out)
		}
		return string(out)
	}
	if out := prepare(); out != "" {
		t.Errorf("first prepare printed %q", out)
	}
	if target, err := os.Readlink(home + "/workspace"); err != nil || target != root {
		t.Fatalf("~/workspace -> %q, %v; want %q", target, err, root)
	}
	if out := prepare(); out != "" {
		t.Errorf("prepare with the link in place printed %q", out)
	}
	os.Remove(home + "/workspace")
	if err := os.Mkdir(home+"/workspace", 0o755); err != nil {
		t.Fatal(err)
	}
	if out := prepare(); !strings.Contains(out, "not a link to "+root) {
		t.Errorf("prepare over an existing ~/workspace printed %q", out)
	}
	if fi, err := os.Lstat(home + "/workspace"); err != nil || !fi.IsDir() {
		t.Errorf("existing ~/workspace was changed: %v, %v", fi, err)
	}
}
