package workspace

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckUploadSource(t *testing.T) {
	home := t.TempDir()
	pvmHome := filepath.Join(home, ".pvm")
	project := filepath.Join(home, "src", "app")
	write(t, project, "main.go", "package main\n")
	if err := os.MkdirAll(pvmHome, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := CheckUploadSource(project, []string{home}, pvmHome, nil); err != nil {
		t.Fatalf("project refused: %v", err)
	}
	refused := []struct{ dir, want string }{
		{home, "home directory"},
		{filepath.Dir(home), "contains your home"},
		{filepath.VolumeName(home) + string(filepath.Separator), "file system root"},
		{pvmHome, "inside pvm's data"},
		{filepath.Join(pvmHome, "vms"), "inside pvm's data"},
	}
	if err := os.MkdirAll(filepath.Join(pvmHome, "vms"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, c := range refused {
		err := CheckUploadSource(c.dir, []string{home}, pvmHome, nil)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("CheckUploadSource(%s) = %v, want %q", c.dir, err, c.want)
		}
	}

	// pvm's data directory below the folder, with home elsewhere.
	other := t.TempDir()
	if err := os.MkdirAll(filepath.Join(other, "state"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := CheckUploadSource(other, []string{home}, filepath.Join(other, "state"), nil)
	if err == nil || !strings.Contains(err.Error(), "contains pvm's data") {
		t.Errorf("ancestor of pvm home: %v", err)
	}

	// Every home counts, such as the Windows profile under WSL.
	err = CheckUploadSource(other, []string{home, other}, pvmHome, nil)
	if err == nil || !strings.Contains(err.Error(), "home directory") {
		t.Errorf("second home: %v", err)
	}
}

func TestWSLDrives(t *testing.T) {
	old := inWSL
	t.Cleanup(func() { inWSL = old })
	inWSL = func() bool { return true }
	for _, p := range []string{"/mnt/c", "/mnt/D"} {
		if !wslDriveRoot.MatchString(p) || !folds(p) {
			t.Errorf("%s: not a case-insensitive drive root", p)
		}
	}
	if !folds("/mnt/c/Users/dev") || wslDriveRoot.MatchString("/mnt/c/Users") || wslDriveRoot.MatchString("/mnt/cd") {
		t.Error("drive patterns match the wrong paths")
	}
	if !within(filepath.FromSlash("/mnt/c/users/dev/proj"), filepath.FromSlash("/mnt/c/Users/Dev")) {
		t.Error("drive paths compared case-sensitively")
	}
	inWSL = func() bool { return false }
	if folds("/mnt/c/x") != caseInsensitiveFS() {
		t.Error("drive rules apply outside WSL")
	}
}

func TestCheckUploadSourceCredentials(t *testing.T) {
	home := t.TempDir()
	for cred, credPath := range map[string]string{
		"a/.ssh/id": "a/.ssh", ".gnupg/k": ".gnupg", ".aws/config": ".aws", ".kube/config": ".kube",
		".docker/config.json": ".docker", "b/.netrc": "b/.netrc", ".git-credentials": ".git-credentials",
	} {
		project := t.TempDir()
		write(t, project, "main.go", "x")
		write(t, project, cred, "x")
		none, err := NewMatcher(nil)
		if err != nil {
			t.Fatal(err)
		}
		err = CheckUploadSource(project, []string{home}, filepath.Join(home, ".pvm"), none)
		if err == nil || !strings.Contains(err.Error(), credPath) || !strings.Contains(err.Error(), ".pvmignore") {
			t.Errorf("%s: err = %v", cred, err)
			continue
		}
		m, err := NewMatcher(map[string][]byte{".pvmignore": []byte("/" + credPath + "\n")})
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckUploadSource(project, []string{home}, filepath.Join(home, ".pvm"), m); err != nil {
			t.Errorf("%s excluded but still refused: %v", cred, err)
		}
		// A mount shares ignored files too, so ignore rules cannot help.
		if err := CheckUploadSource(project, []string{home}, filepath.Join(home, ".pvm"), nil); err == nil || !strings.Contains(err.Error(), "mount shares every file") {
			t.Errorf("%s in a mount: err = %v", cred, err)
		}
	}
}
