package sftpd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVCSMetadataIsReadOnlyOverSFTP(t *testing.T) {
	c, root, _ := fixture(t, false)
	if err := os.MkdirAll(filepath.Join(root, ".git", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte("[core]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got, err := readFile(c, ".git/config"); err != nil || got != "[core]\n" {
		t.Fatalf("reading .git/config must stay allowed: %q, %v", got, err)
	}
	writeFile(t, c, "main.go", "package main\n")

	for _, p := range []string{".git/hooks/pre-commit", ".git/config", ".GIT/config", "sub/.hg/hgrc", "sub/.jj/config.toml"} {
		if err := c.MkdirAll(filepath.Dir(p)); err == nil && filepath.Dir(p) != "." && filepath.Base(filepath.Dir(p)) != "hooks" && filepath.Dir(p) != ".git" {
			// creating a .git/.hg directory itself must also fail
		}
		if f, err := c.Create(p); err == nil {
			f.Close()
			t.Errorf("write to protected VCS path %q succeeded", p)
		}
	}
	if err := c.Mkdir(".git2"); err != nil {
		t.Fatalf("non-metadata dir .git2 must be allowed: %v", err)
	}
	if err := c.Rename("main.go", ".git/hooks/post-checkout"); err == nil {
		t.Fatal("rename into .git/hooks succeeded")
	}
	if err := c.Remove(".git/config"); err == nil {
		t.Fatal("remove of .git/config succeeded")
	}
	if got, err := os.ReadFile(filepath.Join(root, ".git", "config")); err != nil || string(got) != "[core]\n" {
		t.Fatalf("host .git/config modified: %q, %v", got, err)
	}
}
