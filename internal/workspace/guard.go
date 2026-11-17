package workspace

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var credentialNames = map[string]bool{
	".ssh": true, ".gnupg": true, ".aws": true, ".kube": true, ".docker": true,
	".netrc": true, ".git-credentials": true,
}

// CheckUploadSource refuses host folders whose upload would send the
// user's personal files or pvm's own state into a VM. A nil m means every
// file is shared, as in a mount.
func CheckUploadSource(root string, homes []string, pvmHome string, m Matcher) error {
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	dir, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return err
	}
	refuse := func(why string) error {
		return fmt.Errorf("refusing to upload %s: %s; choose a project folder instead", dir, why)
	}
	if filepath.Dir(dir) == dir || inWSL() && wslDriveRoot.MatchString(dir) {
		return refuse("it is a file system root")
	}
	for _, home := range homes {
		h := resolve(home)
		if samePath(dir, h) {
			return refuse("it is your home directory")
		}
		if within(h, dir) {
			return refuse("it contains your home directory")
		}
	}
	if pvmHome != "" {
		p := resolve(pvmHome)
		if within(dir, p) {
			return refuse("it is inside pvm's data directory " + p)
		}
		if within(p, dir) {
			return refuse("it contains pvm's data directory " + p)
		}
	}
	fold := folds(dir)
	if isCredentialName(filepath.Base(dir), fold) {
		return refuse("it is a credential directory")
	}
	var found []string
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		skip := func() error {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if m != nil && m.Excluded(rel, d.IsDir()) {
			return skip()
		}
		if isCredentialName(d.Name(), fold) {
			found = append(found, rel)
			return skip()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(found) > 0 && m == nil {
		return fmt.Errorf("refusing to share %s: it contains credentials at %s, and a mount shares every file; choose another folder",
			dir, strings.Join(found, ", "))
	}
	if len(found) > 0 {
		return fmt.Errorf("refusing to upload %s: it contains credentials at %s; exclude them in %s (for example a line \"/%s\") or choose another folder",
			dir, strings.Join(found, ", "), filepath.Join(dir, pvmignoreName), found[0])
	}
	return nil
}

func isCredentialName(name string, fold bool) bool {
	if fold {
		name = strings.ToLower(name)
	}
	return credentialNames[name]
}

func caseInsensitiveFS() bool { return runtime.GOOS == "windows" || runtime.GOOS == "darwin" }

// Under WSL, Windows drives are case-insensitive mounts below /mnt.
var (
	wslDrive     = regexp.MustCompile(`^/mnt/[A-Za-z](/|$)`)
	wslDriveRoot = regexp.MustCompile(`^/mnt/[A-Za-z]$`)
)

var inWSL = func() bool { return runtime.GOOS == "linux" && os.Getenv("WSL_DISTRO_NAME") != "" }

// folds reports whether names below p compare case-insensitively.
func folds(p string) bool { return caseInsensitiveFS() || inWSL() && wslDrive.MatchString(p) }

// resolve returns the real path, or the absolute path when it cannot be
// resolved, for example because it does not exist yet.
func resolve(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

func foldPath(p string) string {
	if folds(p) {
		return strings.ToLower(p)
	}
	return p
}

func samePath(a, b string) bool { return foldPath(a) == foldPath(b) }

// within reports whether child is parent or lies below it.
func within(child, parent string) bool {
	c, p := foldPath(child), foldPath(parent)
	if c == p {
		return true
	}
	if !strings.HasSuffix(p, string(filepath.Separator)) {
		p += string(filepath.Separator)
	}
	return strings.HasPrefix(c, p)
}
