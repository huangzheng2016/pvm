package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const (
	gitignoreName = ".gitignore"
	pvmignoreName = ".pvmignore"
)

// IgnoreMatcher applies .gitignore and .pvmignore files with git
// semantics. The zero value excludes nothing.
type IgnoreMatcher struct {
	rules []ignoreRule // lowest precedence first
}

type ignoreRule struct {
	base    []string // directory of the ignore file
	segs    []string // pattern split on "/"; "**" matches any number of segments
	negate  bool
	dirOnly bool
}

// NewMatcher builds a matcher from ignore file contents keyed by slash
// path relative to the workspace root. Later rules win: the built-in
// node_modules rule, then .gitignore files from shallow to deep, then
// .pvmignore files from shallow to deep, so .pvmignore can re-include.
func NewMatcher(files map[string][]byte) (*IgnoreMatcher, error) {
	m := &IgnoreMatcher{}
	if err := m.add(nil, "node_modules/"); err != nil {
		return nil, err
	}
	for _, kind := range []string{gitignoreName, pvmignoreName} {
		var names []string
		for name := range files {
			if path.Base(name) == kind {
				names = append(names, name)
			}
		}
		sort.Slice(names, func(i, j int) bool {
			di, dj := strings.Count(names[i], "/"), strings.Count(names[j], "/")
			if di != dj {
				return di < dj
			}
			return names[i] < names[j]
		})
		for _, name := range names {
			var base []string
			if dir := path.Dir(name); dir != "." {
				base = strings.Split(dir, "/")
			}
			for i, line := range strings.Split(string(files[name]), "\n") {
				if err := m.add(base, line); err != nil {
					return nil, fmt.Errorf("%s:%d: %w", name, i+1, err)
				}
			}
		}
	}
	return m, nil
}

func (m *IgnoreMatcher) add(base []string, line string) error {
	line = strings.TrimSuffix(line, "\r")
	// Trailing spaces are ignored unless escaped with a backslash.
	for strings.HasSuffix(line, " ") && !strings.HasSuffix(line, `\ `) {
		line = line[:len(line)-1]
	}
	if line == "" || line[0] == '#' {
		return nil
	}
	r := ignoreRule{base: base}
	if line[0] == '!' {
		r.negate = true
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		r.dirOnly = true
		line = strings.TrimRight(line, "/")
	}
	if line == "" {
		return nil
	}
	anchored := strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")
	for _, s := range strings.Split(line, "/") {
		if s == "" {
			continue
		}
		if s != "**" {
			// path.Match only knows [^...]; git also accepts [!...].
			s = strings.ReplaceAll(s, "[!", "[^")
			if _, err := path.Match(s, ""); err != nil {
				return fmt.Errorf("bad pattern %q", line)
			}
		}
		r.segs = append(r.segs, s)
	}
	if len(r.segs) == 0 {
		return nil
	}
	if !anchored {
		r.segs = append([]string{"**"}, r.segs...)
	}
	m.rules = append(m.rules, r)
	return nil
}

// Excluded reports whether rel is excluded. As in git, nothing below an
// excluded directory can be re-included.
func (m *IgnoreMatcher) Excluded(rel string, isDir bool) bool {
	if m == nil || rel == "" || rel == "." {
		return false
	}
	segs := strings.Split(rel, "/")
	for i := 1; i < len(segs); i++ {
		if m.match(segs[:i], true) {
			return true
		}
	}
	return m.match(segs, isDir)
}

func (m *IgnoreMatcher) match(segs []string, isDir bool) bool {
	for i := len(m.rules) - 1; i >= 0; i-- {
		r := m.rules[i]
		if r.dirOnly && !isDir {
			continue
		}
		if len(segs) <= len(r.base) || !equalSegs(segs[:len(r.base)], r.base) {
			continue
		}
		if matchSegs(r.segs, segs[len(r.base):]) {
			return !r.negate
		}
	}
	return false
}

func equalSegs(a, b []string) bool {
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func matchSegs(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			rest := pat[1:]
			// A trailing "/**" matches everything inside, not the directory itself.
			if len(rest) == 0 {
				return len(segs) > 0
			}
			for i := 0; i <= len(segs); i++ {
				if matchSegs(rest, segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, _ := path.Match(pat[0], segs[0]); !ok {
			return false
		}
		pat, segs = pat[1:], segs[1:]
	}
	return len(segs) == 0
}

// LoadMatcher reads every .gitignore and .pvmignore below root, skipping
// excluded directories and .git, without following symlinks.
func LoadMatcher(root string) (*IgnoreMatcher, error) {
	files := map[string][]byte{}
	m, err := NewMatcher(files)
	if err != nil {
		return nil, err
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel != "." && (d.Name() == ".git" || m.Excluded(rel, true)) {
			return filepath.SkipDir
		}
		found := false
		for _, name := range []string{gitignoreName, pvmignoreName} {
			f := filepath.Join(p, name)
			fi, err := os.Lstat(f)
			if errors.Is(err, fs.ErrNotExist) || err == nil && !fi.Mode().IsRegular() {
				continue
			}
			if err != nil {
				return err
			}
			b, err := os.ReadFile(f)
			if err != nil {
				return err
			}
			key := name
			if rel != "." {
				key = rel + "/" + name
			}
			files[key] = b
			found = true
		}
		if found {
			if m, err = NewMatcher(files); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return m, nil
}
