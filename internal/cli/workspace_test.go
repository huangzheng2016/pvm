package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/huangzheng2016/pvm/internal/paths"
	"github.com/huangzheng2016/pvm/internal/workspace"
	"github.com/huangzheng2016/pvm/internal/workspace/guest"
	"github.com/huangzheng2016/pvm/internal/workspace/threeway"
)

const testGP = "/workspace/web"

// fakeGuest serves the guest commands from a local directory, so the
// transfer code runs end to end without a VM.
type fakeGuest struct {
	t    *testing.T
	dir  string
	cmds []string
}

func (f *fakeGuest) guest() *wsGuest { return &wsGuest{vm: "dev1", run: f.run} }

func mustCmd(cmd string, err error) string {
	if err != nil {
		panic(err)
	}
	return cmd
}

func splitNUL(b []byte) []string {
	s := strings.TrimSuffix(string(b), "\x00")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\x00")
}

func (f *fakeGuest) run(ctx context.Context, cmd string, stdin io.Reader, stdout io.Writer) error {

	in, err := io.ReadAll(stdin)
	if err != nil {
		return err
	}
	switch cmd {
	case mustCmd(guest.ListCommand(testGP)):
		f.cmds = append(f.cmds, "list")
		man, _, err := workspace.ScanHost(f.dir, nil, nil)
		if err != nil {
			return err
		}
		for _, e := range man.Sorted() {
			y, mode := map[workspace.EntryType]string{workspace.TypeFile: "f", workspace.TypeDir: "d", workspace.TypeSymlink: "l"}[e.Type], "644"
			if e.Exec {
				mode = "755"
			}
			fmt.Fprintf(stdout, "%s\x00%d\x00%d.%09d\x00%s\x00%s\x00%s\x00", y, e.Size, e.MTime/1e9, e.MTime%1e9, mode, e.Link, e.Path)
		}
	case mustCmd(guest.HashCommand(testGP)):
		names := splitNUL(in)
		f.cmds = append(f.cmds, "hash "+strings.Join(names, ","))
		man, _, err := workspace.ScanHost(f.dir, nil, nil)
		if err != nil {
			return err
		}
		for _, n := range names {
			fmt.Fprintf(stdout, "%s  %s\x00", man[n].SHA256, n)
		}
	case mustCmd(guest.DownloadCommand(testGP)):
		names := splitNUL(in)
		f.cmds = append(f.cmds, "download "+strings.Join(names, ","))
		man, _, err := workspace.ScanHost(f.dir, nil, nil)
		if err != nil {
			return err
		}
		var ents []workspace.Entry
		for _, n := range names {
			ents = append(ents, man[n])
		}
		return workspace.WriteTar(stdout, f.dir, ents, "")
	case mustCmd(guest.UploadCommand(testGP, false)):
		f.cmds = append(f.cmds, "upload")
		return f.upload(bytes.NewReader(in))
	case mustCmd(guest.RenameCommand(testGP)):
		pairs := splitNUL(in)
		f.cmds = append(f.cmds, "rename "+strings.Join(pairs, ","))
		for i := 0; i+1 < len(pairs); i += 2 {
			if err := os.Rename(filepath.Join(f.dir, pairs[i]), filepath.Join(f.dir, pairs[i+1])); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected guest command %q", cmd)
	}
	return nil
}

func (f *fakeGuest) upload(r io.Reader) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if hdr.Name == "pvm/deletes" {
			data, _ := io.ReadAll(tr)
			for _, d := range splitNUL(data) {
				if err := os.Remove(filepath.Join(f.dir, d)); err != nil {
					return err
				}
			}
			continue
		}
		rel, ok := strings.CutPrefix(hdr.Name, "files/")
		if !ok {
			return fmt.Errorf("unexpected upload entry %q", hdr.Name)
		}
		dst := filepath.Join(f.dir, strings.TrimSuffix(rel, "/"))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			data, _ := io.ReadAll(tr)
			if err := os.WriteFile(dst, data, 0o644); err != nil {
				return err
			}
		}
	}
}

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, data := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	man, _, err := workspace.ScanHost(root, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for p, e := range man {
		if e.Type == workspace.TypeFile {
			b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
			if err != nil {
				t.Fatal(err)
			}
			out[p] = string(b)
		}
	}
	return out
}

// syncFixture creates a host and a guest tree that agree on base, records
// the baseline, then applies the given changes.
func syncFixture(t *testing.T, base, hostChanges, guestChanges map[string]string, guestDeletes []string) (*syncState, *fakeGuest) {
	t.Helper()
	hostDir, guestDir := t.TempDir(), t.TempDir()
	writeFiles(t, hostDir, base)
	writeFiles(t, guestDir, base)
	fg := &fakeGuest{t: t, dir: guestDir}
	g := fg.guest()
	h, _, err := workspace.ScanHost(hostDir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	gs, _, err := g.scan(testGP, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	synced := map[string]bool{}
	for p := range h {
		synced[p] = true
	}
	bl := threeway.NextBaseline(nil, h, gs, synced)

	// Make sure rewritten files get a different mtime on coarse clocks.
	later := time.Now().Add(time.Minute)
	writeFiles(t, hostDir, hostChanges)
	writeFiles(t, guestDir, guestChanges)
	for rel := range guestChanges {
		os.Chtimes(filepath.Join(guestDir, rel), later, later)
	}
	for rel := range hostChanges {
		os.Chtimes(filepath.Join(hostDir, rel), later, later)
	}
	for _, d := range guestDeletes {
		if err := os.Remove(filepath.Join(guestDir, d)); err != nil {
			t.Fatal(err)
		}
	}
	h, _, err = workspace.ScanHost(hostDir, nil, bl.Host)
	if err != nil {
		t.Fatal(err)
	}
	fg.cmds = nil
	gs, _, err = g.scan(testGP, nil, bl.Guest)
	if err != nil {
		t.Fatal(err)
	}
	ws := &workspace.Workspace{ID: "web", VM: "dev1", Mode: workspace.ModeSync, HostPath: hostDir, GuestPath: testGP}
	return &syncState{ws: ws, g: g, base: bl, host: h, guest: gs}, fg
}

func TestWorkspaceGuestScanRehashesOnlyChanged(t *testing.T) {
	_, fg := syncFixture(t, map[string]string{"a.txt": "one", "b.txt": "same"}, nil, map[string]string{"a.txt": "two!"}, nil)
	if want := []string{"list", "hash a.txt"}; !reflect.DeepEqual(fg.cmds, want) {
		t.Fatalf("guest commands = %q, want %q", fg.cmds, want)
	}
}

func TestWorkspacePullApplies(t *testing.T) {
	s, _ := syncFixture(t,
		map[string]string{"a.txt": "one", "sub/b.txt": "b", "keep.txt": "k"},
		nil,
		map[string]string{"a.txt": "two", "sub/c.txt": "c", "new/d.txt": "d"},
		[]string{"sub/b.txt"})
	pl := threeway.NewPlan(s.base, s.host, s.guest, threeway.Pull, threeway.None)
	if pl.Unresolved {
		t.Fatalf("unexpected conflicts: %+v", pl.Conflicts)
	}
	written, rejected, err := s.apply(pl)
	if err != nil || len(rejected) > 0 {
		t.Fatalf("apply: %v, rejected %v", err, rejected)
	}
	if len(written) == 0 {
		t.Fatal("nothing written")
	}
	want := map[string]string{"a.txt": "two", "sub/c.txt": "c", "new/d.txt": "d", "keep.txt": "k"}
	if got := readTree(t, s.ws.HostPath); !reflect.DeepEqual(got, want) {
		t.Fatalf("host tree = %v, want %v", got, want)
	}
}

func TestWorkspaceConflictsKeepBoth(t *testing.T) {
	s, fg := syncFixture(t,
		map[string]string{"a.txt": "one"},
		map[string]string{"a.txt": "host", "h.txt": "h"},
		map[string]string{"a.txt": "vm"},
		nil)
	if pl := threeway.NewPlan(s.base, s.host, s.guest, threeway.Push, threeway.None); !pl.Unresolved {
		t.Fatal("conflict not detected")
	}
	pl := threeway.NewPlan(s.base, s.host, s.guest, threeway.Push, threeway.KeepBoth)
	if _, rejected, err := s.apply(pl); err != nil || len(rejected) > 0 {
		t.Fatalf("apply: %v, rejected %v", err, rejected)
	}
	want := map[string]string{"a.txt": "host", "a.txt.pvm-conflict-vm": "vm", "h.txt": "h"}
	if got := readTree(t, s.ws.HostPath); !reflect.DeepEqual(got, want) {
		t.Fatalf("host tree = %v, want %v", got, want)
	}
	if got := readTree(t, fg.dir); !reflect.DeepEqual(got, want) {
		t.Fatalf("guest tree = %v, want %v", got, want)
	}
}

func TestWorkspacePullRejectsChangedFile(t *testing.T) {
	s, fg := syncFixture(t, map[string]string{"a.txt": "one"}, nil, map[string]string{"a.txt": "two"}, nil)
	pl := threeway.NewPlan(s.base, s.host, s.guest, threeway.Pull, threeway.None)
	writeFiles(t, fg.dir, map[string]string{"a.txt": "three"})
	_, rejected, err := s.apply(pl)
	if err == nil && len(rejected) == 0 {
		t.Fatal("a file that changed after the scan must not be accepted silently")
	}
	if got := readTree(t, s.ws.HostPath)["a.txt"]; got != "one" {
		t.Fatalf("host a.txt = %q, want the old content", got)
	}
}

func TestWorkspaceUploadFailureCorruptsStream(t *testing.T) {
	var got []byte
	g := &wsGuest{vm: "dev1", run: func(_ context.Context, _ string, stdin io.Reader, _ io.Writer) error {
		got, _ = io.ReadAll(stdin)
		return nil
	}}
	boom := errors.New("boom")
	err := g.upload("x", func(w io.Writer) error {
		w.Write(make([]byte, 512))
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("upload error = %v, want the producer's", err)
	}
	if len(got) < 1024 || got[len(got)-1] != 0xff {
		t.Fatalf("failed stream must end in a corrupt block, got %d bytes", len(got))
	}
}

func TestWorkspaceUploadGuestExitsEarly(t *testing.T) {
	g := &wsGuest{vm: "dev1", run: func(context.Context, string, io.Reader, io.Writer) error {
		return errors.New("dev1: connection refused")
	}}
	done := make(chan error, 1)
	go func() {
		done <- g.upload("x", func(w io.Writer) error {
			_, err := w.Write(make([]byte, 1<<20))
			return err
		})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("upload succeeded although the guest command failed")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("upload hangs when the guest command exits early")
	}
}

func TestWorkspaceGuestMatcher(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		".gitignore":                "dist/\n",
		".pvmignore":                "!keep.log\n*.log\n",
		"src/main.go":               "package main",
		"dist/out.js":               "x",
		"node_modules/x/.pvmignore": "",
		"node_modules/x/i.js":       "x",
	})
	fg := &fakeGuest{t: t, dir: dir}
	g := fg.guest()
	all, skipped, err := g.list(testGP)
	if err != nil {
		t.Fatal(err)
	}
	m, err := g.matcher(testGP, all)
	if err != nil {
		t.Fatal(err)
	}
	man, _, err := g.complete(testGP, all, skipped, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{".gitignore", ".pvmignore", "src", "src/main.go"} {
		if _, ok := man[p]; !ok {
			t.Errorf("%s missing", p)
		}
	}
	for _, p := range []string{"dist", "dist/out.js", "node_modules", "node_modules/x/i.js"} {
		if _, ok := man[p]; ok {
			t.Errorf("%s not excluded", p)
		}
	}
}

func TestWriteUploadDeletesFirst(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a.txt": "a"})
	man, _, err := workspace.ScanHost(dir, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := writeUpload(&buf, dir, man.Sorted(), []string{"x/y", "x"}); err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(&buf)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
		if hdr.Name == "pvm/deletes" {
			data, _ := io.ReadAll(tr)
			if string(data) != "x/y\x00x\x00" {
				t.Fatalf("deletes = %q", data)
			}
		}
	}
	if want := []string{"pvm/deletes", "files/a.txt"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("entries = %q, want %q", names, want)
	}
}

func TestParseInterleaved(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	mode := fs.String("mode", "", "")
	path := fs.String("path", "", "")
	yes := fs.Bool("y", false, "")
	pos, err := parseInterleaved(fs, []string{"web", "--mode", "sync", "dev1", "--path=/src/web", "-y"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pos, []string{"web", "dev1"}) || *mode != "sync" || *path != "/src/web" || !*yes {
		t.Fatalf("pos=%q mode=%q path=%q y=%v", pos, *mode, *path, *yes)
	}
}

func TestParseResolution(t *testing.T) {
	for _, tc := range []struct {
		prefer string
		keep   bool
		want   threeway.Resolution
		ok     bool
	}{
		{"", false, threeway.None, true},
		{"host", false, threeway.PreferHost, true},
		{"vm", false, threeway.PreferVM, true},
		{"", true, threeway.KeepBoth, true},
		{"vm", true, threeway.None, false},
		{"guest", false, threeway.None, false},
	} {
		got, err := parseResolution(tc.prefer, tc.keep)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("parseResolution(%q, %v) = %v, %v", tc.prefer, tc.keep, got, err)
		}
	}
}

func TestWorkspaceCD(t *testing.T) {
	for _, tc := range []struct {
		name  string
		o     sshOpts
		login bool
		want  string
	}{
		{"no workspace", sshOpts{}, true, ""},
		{"root login", sshOpts{dir: testGP, root: testGP}, true, "cd '/workspace/web' && "},
		{"subdir command", sshOpts{dir: testGP + "/src", root: testGP}, false, "cd '/workspace/web/src' && "},
		{"subdir login falls back", sshOpts{dir: testGP + "/it's", root: testGP}, true,
			`{ cd '/workspace/web/it'"'"'s' 2>/dev/null || { echo 'pvm: /workspace/web/it'"'"'s is not in the vm; starting in /workspace/web' >&2; cd '/workspace/web'; }; } && `},
	} {
		if got := tc.o.workspaceCD(tc.login); got != tc.want {
			t.Errorf("%s: workspaceCD = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestCheckCloneDest(t *testing.T) {
	dir := t.TempDir()
	if _, err := checkCloneDest(filepath.Join(dir, "new")); err != nil {
		t.Errorf("missing dest: %v", err)
	}
	empty := filepath.Join(dir, "empty")
	os.Mkdir(empty, 0o755)
	if _, err := checkCloneDest(empty); err != nil {
		t.Errorf("empty dest: %v", err)
	}
	writeFiles(t, dir, map[string]string{"full/x": "x", "file": "f"})
	if _, err := checkCloneDest(filepath.Join(dir, "full")); err == nil {
		t.Error("non-empty dest accepted")
	}
	if _, err := checkCloneDest(filepath.Join(dir, "file")); err == nil {
		t.Error("file dest accepted")
	}
}

func TestRenameInRootRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"a": "a", "b": "b"})
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := renameInRoot(root, "a", "b"); err == nil {
		t.Fatal("rename over an existing path succeeded")
	}
	if err := renameInRoot(root, "a", "c"); err != nil {
		t.Fatal(err)
	}
}

func TestVMWorkspaceNotes(t *testing.T) {
	p := paths.New(t.TempDir())
	store := workspace.NewStore(p)
	for _, ws := range []*workspace.Workspace{
		{ID: "web", VM: "dev1", Mode: workspace.ModeSync, HostPath: "/src/web", GuestPath: "/workspace/web"},
		{ID: "docs", VM: "dev1", Mode: workspace.ModeMount, HostPath: "/src/docs", GuestPath: "/workspace/docs"},
		{ID: "other", VM: "dev2", Mode: workspace.ModeCopy, HostPath: "/src/other", GuestPath: "/workspace/other"},
	} {
		if err := store.Save(ws); err != nil {
			t.Fatal(err)
		}
	}
	notes := vmWorkspaceNotes(p, "dev1", func(ws *workspace.Workspace) ([]threeway.Change, error) {
		if ws.ID != "web" {
			t.Errorf("unsaved check for %s", ws.ID)
		}
		return []threeway.Change{{Path: "a", Kind: threeway.Modified}}, nil
	})
	text := strings.Join(notes, "\n")
	for _, want := range []string{"workspace docs (mount)", "workspace web (sync)", "1 changes", "pvm workspace pull web", "orphans"} {
		if !strings.Contains(text, want) {
			t.Errorf("notes missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "other") {
		t.Errorf("notes list another VM's workspace:\n%s", text)
	}
	if notes := vmWorkspaceNotes(p, "dev3", nil); len(notes) != 0 {
		t.Errorf("notes for a VM without workspaces: %q", notes)
	}
}
