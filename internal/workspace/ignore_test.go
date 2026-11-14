package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIgnoreMatcher(t *testing.T) {
	m, err := NewMatcher(map[string][]byte{
		".gitignore":     []byte("# comment\n\n/build\n*.log\n!keep.log\ndist/\ndocs/*.tmp\n\\#hash\nout/**\n**/cache\n.env\n"),
		"sub/.gitignore": []byte("local\n/anchored\n!/root.log\n"),
		".pvmignore":     []byte("!.env\n!/vendor/node_modules/\nsecret[0-9].txt\nextra/\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		rel   string
		dir   bool
		want  bool
		label string
	}{
		{"build", true, true, "anchored"},
		{"build", false, true, "anchored file"},
		{"sub/build", true, false, "leading slash anchors to root"},
		{"a.log", false, true, "glob"},
		{"x/y/a.log", false, true, "unanchored glob at depth"},
		{"keep.log", false, false, "negation"},
		{"dist", true, true, "dir-only"},
		{"dist", false, false, "dir-only skips files"},
		{"dist/a.go", false, true, "inside excluded dir"},
		{"docs/a.tmp", false, true, "middle slash anchors"},
		{"x/docs/a.tmp", false, false, "middle slash anchored"},
		{"#hash", false, true, "escaped hash"},
		{"out", true, false, "trailing ** spares the dir"},
		{"out/a/b", false, true, "trailing ** matches inside"},
		{"a/b/cache", true, true, "leading **"},
		{"sub/local", false, true, "nested gitignore"},
		{"local", false, false, "nested gitignore only below its dir"},
		{"sub/anchored", false, true, "nested anchored"},
		{"sub/x/anchored", false, false, "nested anchored to its dir"},
		{"sub/root.log", false, false, "nested negation beats parent"},
		{"node_modules", true, true, "built-in node_modules"},
		{"a/node_modules/x.js", false, true, "nested node_modules"},
		{"vendor/node_modules", true, false, "pvmignore re-includes dir"},
		{"vendor/node_modules/x.js", false, false, "inside re-included dir"},
		{".env", false, false, "pvmignore re-includes file"},
		{"secret1.txt", false, true, "pvmignore adds pattern"},
		{"secretx.txt", false, false, "character class"},
		{"extra/f", false, true, "pvmignore dir"},
		{".git", true, false, ".git is kept"},
		{".git/config", false, false, ".git contents kept"},
	}
	for _, c := range cases {
		if got := m.Excluded(c.rel, c.dir); got != c.want {
			t.Errorf("%s: Excluded(%q, %v) = %v, want %v", c.label, c.rel, c.dir, got, c.want)
		}
	}
}

func TestIgnoreNoReincludeInsideExcludedDir(t *testing.T) {
	m, err := NewMatcher(map[string][]byte{
		".gitignore": []byte("build/\n!build/keep\n"),
		".pvmignore": []byte("!build/also\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"build/keep", "build/also"} {
		if !m.Excluded(rel, false) {
			t.Errorf("%s re-included inside an excluded directory", rel)
		}
	}
}

func TestIgnoreBadPattern(t *testing.T) {
	if _, err := NewMatcher(map[string][]byte{".gitignore": []byte("[a\n")}); err == nil {
		t.Fatal("unterminated class accepted")
	}
}

func TestLoadMatcher(t *testing.T) {
	root := t.TempDir()
	write(t, root, ".gitignore", "ignored/\n")
	write(t, root, "ignored/.gitignore", "!*\n")
	write(t, root, "ignored/.pvmignore", "!*\n")
	write(t, root, "sub/.gitignore", "*.o\n")
	write(t, root, "sub/.pvmignore", "!keep.o\n")
	m, err := LoadMatcher(root)
	if err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]bool{
		"ignored/x":    true,
		"sub/a.o":      true,
		"sub/keep.o":   false,
		"a.o":          false,
		"node_modules": true,
	} {
		if got := m.Excluded(rel, rel == "node_modules"); got != want {
			t.Errorf("Excluded(%q) = %v, want %v", rel, got, want)
		}
	}
}

func write(t *testing.T, root, rel, data string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}
