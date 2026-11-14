package workspace

import (
	"archive/tar"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

type ExtractOptions struct {
	// Overwrite replaces existing paths, writing files to a temporary
	// name and renaming them into place. Without it files are created
	// exclusively and existing paths fail the extraction.
	Overwrite bool
	// Only, when non-nil, rejects every entry not listed.
	Only map[string]bool
	// Expect, when non-nil, rejects files whose hash differs from it.
	Expect          Manifest
	StripPrefix     string
	WindowsNames    bool
	CaseInsensitive bool
}

func DefaultExtractOptions() ExtractOptions {
	return ExtractOptions{
		WindowsNames:    runtime.GOOS == "windows",
		CaseInsensitive: caseInsensitiveFS(),
	}
}

type ExtractResult struct {
	Written         []string
	Rejected        []Skipped
	SkippedSymlinks []string
}

// ExtractTar writes an untrusted archive into dest. Unsafe entries are
// rejected and listed instead of written; I/O failures stop extraction.
func ExtractTar(r io.Reader, dest *os.Root, opts ExtractOptions) (*ExtractResult, error) {
	res := &ExtractResult{}
	seen := map[string]bool{}
	tree := &destTree{root: dest, fold: opts.CaseInsensitive, dirs: map[string]map[string]destEnt{}}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return res, nil
		}
		if err != nil {
			return res, err
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name := hdr.Name
		reject := func(reason string) {
			res.Rejected = append(res.Rejected, Skipped{Path: name, Reason: reason})
		}
		if opts.StripPrefix != "" {
			rest, ok := strings.CutPrefix(name, opts.StripPrefix)
			if !ok {
				reject("outside the expected directory " + opts.StripPrefix)
				continue
			}
			name = rest
		}
		if hdr.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
			if name == "" || name == "." {
				continue // the archive root
			}
		}
		if reason := checkName(name, opts.WindowsNames); reason != "" {
			reject(reason)
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeReg, tar.TypeDir:
		case tar.TypeSymlink:
			if reason := checkLink(name, hdr.Linkname); reason != "" {
				reject(reason)
				continue
			}
		default:
			reject(fmt.Sprintf("unsupported entry type %q (only files, directories and symlinks are copied)", hdr.Typeflag))
			continue
		}
		if opts.Only != nil && !opts.Only[name] {
			reject("not part of this transfer")
			continue
		}
		if seen[name] {
			reject("duplicate entry")
			continue
		}
		seen[name] = true
		if opts.Expect != nil && hdr.Typeflag != tar.TypeReg {
			// Files are checked against their hash once read.
			e, ok := opts.Expect[name]
			if !ok || (hdr.Typeflag == tar.TypeDir) != (e.Type == TypeDir) || hdr.Typeflag == tar.TypeSymlink && (e.Type != TypeSymlink || e.Link != hdr.Linkname) {
				reject("changed since it was scanned; rerun the command")
				continue
			}
		}
		reason, err := tree.check(name)
		if err != nil {
			return res, fmt.Errorf("%s: %w", name, err)
		}
		if reason != "" {
			reject(reason)
			continue
		}

		var written bool
		switch hdr.Typeflag {
		case tar.TypeDir:
			err = extractDir(dest, name, opts.Overwrite)
			written = err == nil
		case tar.TypeSymlink:
			written, err = extractSymlink(dest, name, hdr.Linkname, opts.Overwrite)
			if err == nil && !written {
				res.SkippedSymlinks = append(res.SkippedSymlinks, name)
			}
		case tar.TypeReg:
			var reason string
			reason, err = extractFile(dest, name, hdr, tr, opts)
			if reason != "" {
				reject(reason)
			}
			written = err == nil && reason == ""
		}
		if err != nil {
			return res, fmt.Errorf("%s: %w", name, err)
		}
		if written {
			tree.add(name, hdr.Typeflag == tar.TypeDir)
			res.Written = append(res.Written, name)
		}
	}
}

// checkName returns why a slash path from an untrusted archive is unsafe,
// or "" when it is fine.
func checkName(name string, windowsNames bool) string {
	if name == "" {
		return "empty name"
	}
	if strings.HasPrefix(name, "/") {
		return "absolute path"
	}
	if strings.Contains(name, `\`) {
		return "backslash in name"
	}
	for _, c := range strings.Split(name, "/") {
		switch c {
		case "", ".":
			return "empty or '.' path component"
		case "..":
			return "'..' path component"
		}
		if windowsNames {
			if reason := windowsNameProblem(c); reason != "" {
				return reason
			}
		}
	}
	return ""
}

var reservedNames = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true}

func init() {
	for i := '0'; i <= '9'; i++ {
		reservedNames["COM"+string(i)] = true
		reservedNames["LPT"+string(i)] = true
	}
}

func windowsNameProblem(c string) string {
	for _, r := range c {
		if r < 0x20 || strings.ContainsRune(`<>:"|?*`, r) {
			return fmt.Sprintf("name %q is not allowed on Windows", c)
		}
	}
	if strings.HasSuffix(c, ".") || strings.HasSuffix(c, " ") {
		return fmt.Sprintf("name %q ends with a dot or space, which Windows drops", c)
	}
	base, _, _ := strings.Cut(c, ".")
	if reservedNames[strings.ToUpper(strings.TrimRight(base, " "))] {
		return fmt.Sprintf("name %q is a reserved Windows device name", c)
	}
	return ""
}

func checkLink(name, target string) string {
	if target == "" {
		return "symlink with empty target"
	}
	if strings.HasPrefix(target, "/") || strings.Contains(target, `\`) || filepath.VolumeName(target) != "" {
		return "symlink to an absolute path " + target
	}
	// A ".." after a name could climb out through a symlink that the name
	// resolves to, so only leading ones are allowed. The link's own parents
	// are real directories (destTree.check), so counting them is exact.
	depth, leading := strings.Count(name, "/"), true
	for _, c := range strings.Split(target, "/") {
		switch c {
		case "", ".":
		case "..":
			if !leading {
				return "symlink target has '..' after a name: " + target
			}
			if depth--; depth < 0 {
				return "symlink points outside the workspace: " + target
			}
		default:
			leading = false
		}
	}
	return ""
}

// destTree caches listings of the destination, so entries are checked
// against what is already there and not only against each other.
type destTree struct {
	root *os.Root
	fold bool
	dirs map[string]map[string]destEnt // directory -> key -> entry
}

type destEnt struct {
	name string // spelling on disk
	dir  bool
}

func (t *destTree) key(s string) string {
	if t.fold {
		return strings.ToLower(s)
	}
	return s
}

func (t *destTree) list(dir string) (map[string]destEnt, error) {
	if l, ok := t.dirs[dir]; ok {
		return l, nil
	}
	l := map[string]destEnt{}
	f, err := t.root.Open(dir)
	if err == nil {
		var ents []fs.DirEntry
		ents, err = f.ReadDir(-1)
		f.Close()
		for _, e := range ents {
			l[t.key(e.Name())] = destEnt{e.Name(), e.IsDir()}
		}
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	t.dirs[dir] = l
	return l, nil
}

// check returns why name cannot be written: an existing path that differs
// only by case, or a parent that is not a real directory. os.Root would
// follow a symlinked parent and put the entry, or a relative link target,
// somewhere the plan never looked.
func (t *destTree) check(name string) (string, error) {
	segs := strings.Split(name, "/")
	dir := "."
	for i, s := range segs {
		l, err := t.list(dir)
		if err != nil {
			return "", err
		}
		e, ok := l[t.key(s)]
		if !ok {
			return "", nil
		}
		p := path.Join(dir, e.name)
		if e.name != s {
			return "differs only by case from " + p, nil
		}
		if i < len(segs)-1 && !e.dir {
			return p + " is not a directory (a symlink, file or junction)", nil
		}
		dir = p
	}
	return "", nil
}

// add records a written entry and the parents created for it.
func (t *destTree) add(name string, dir bool) {
	segs := strings.Split(name, "/")
	parent := "."
	for i, s := range segs {
		l, ok := t.dirs[parent]
		if !ok {
			return // read from disk when first needed
		}
		if k := t.key(s); i == len(segs)-1 {
			l[k] = destEnt{s, dir}
		} else if _, ok := l[k]; !ok {
			l[k] = destEnt{s, true}
		}
		parent = path.Join(parent, s)
	}
}

func mkParent(dest *os.Root, name string) error {
	if dir := path.Dir(name); dir != "." {
		return dest.MkdirAll(dir, 0o755)
	}
	return nil
}

// clearForOverwrite removes name unless it already has the wanted type.
// Directories are removed only when empty: the plan deletes the contents
// it knows about first, so anything left is unsynced, such as ignored files.
func clearForOverwrite(dest *os.Root, name string, keep func(fs.FileMode) bool) error {
	fi, err := dest.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if keep(fi.Mode()) {
		return nil
	}
	if err := dest.Remove(name); err != nil {
		if fi.IsDir() {
			return fmt.Errorf("cannot replace directory %s: it holds files pvm does not sync, such as ignored ones; move them away and rerun", name)
		}
		return err
	}
	return nil
}

func extractDir(dest *os.Root, name string, overwrite bool) error {
	if overwrite {
		if err := clearForOverwrite(dest, name, fs.FileMode.IsDir); err != nil {
			return err
		}
	}
	return dest.MkdirAll(name, 0o755)
}

// extractSymlink reports false when the host refused to create the link,
// such as Windows without the symlink privilege.
func extractSymlink(dest *os.Root, name, target string, overwrite bool) (bool, error) {
	if err := mkParent(dest, name); err != nil {
		return false, err
	}
	if overwrite {
		if err := clearForOverwrite(dest, name, func(fs.FileMode) bool { return false }); err != nil {
			return false, err
		}
	}
	err := dest.Symlink(filepath.FromSlash(target), name)
	if errors.Is(err, fs.ErrExist) {
		return false, err
	}
	return err == nil, nil
}

// extractFile returns a rejection reason when the content does not match
// opts.Expect; the partial file is removed in that case.
func extractFile(dest *os.Root, name string, hdr *tar.Header, r io.Reader, opts ExtractOptions) (string, error) {
	if err := mkParent(dest, name); err != nil {
		return "", err
	}
	target := name
	if opts.Overwrite {
		target = name + ".pvm-tmp-" + rand.Text()[:12]
	}
	f, err := dest.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		dest.Remove(target)
		return "", err
	}
	if opts.Expect != nil {
		if e, ok := opts.Expect[name]; !ok || e.Type != TypeFile || e.SHA256 != hex.EncodeToString(h.Sum(nil)) {
			dest.Remove(target)
			return "content changed since it was scanned; rerun the command", nil
		}
	}
	mode := os.FileMode(0o644)
	if hdr.Mode&0o111 != 0 {
		mode = 0o755
	}
	if err := dest.Chmod(target, mode); err != nil && runtime.GOOS != "windows" {
		dest.Remove(target)
		return "", err
	}
	if err := dest.Chtimes(target, hdr.ModTime, hdr.ModTime); err != nil {
		dest.Remove(target)
		return "", err
	}
	if target != name {
		if err := clearForOverwrite(dest, name, fs.FileMode.IsRegular); err != nil {
			dest.Remove(target)
			return "", err
		}
		if err := dest.Rename(target, name); err != nil {
			dest.Remove(target)
			return "", err
		}
	}
	return "", nil
}

// RemoveFromRoot deletes one path for a pull. Directories are removed
// only when empty; a missing path is not an error.
func RemoveFromRoot(root *os.Root, rel string) error {
	if reason := checkName(rel, false); reason != "" {
		return fmt.Errorf("refusing to remove %q: %s", rel, reason)
	}
	err := root.Remove(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// HostExecutable returns the paths that host tools may run or load
// without asking, so the user can review them after a download.
func HostExecutable(paths []string) []string {
	var out []string
	for _, p := range paths {
		l := strings.ToLower(p)
		hook, _ := path.Match(".git/hooks/*", l)
		vscode, _ := path.Match(".vscode/*.json", l)
		if hook || vscode || l == ".git/config" || path.Base(l) == ".envrc" {
			out = append(out, p)
		}
	}
	return out
}
