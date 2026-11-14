package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"unicode/utf8"
)

// Skipped is a path left out of a transfer, with the reason shown to the user.
type Skipped struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// ScanHost walks root without following symlinks. Files whose type, size
// and mtime match prev keep prev's hash instead of being reread.
func ScanHost(root string, m Matcher, prev Manifest) (Manifest, []Skipped, error) {
	out := Manifest{}
	var skipped []Skipped
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !utf8.ValidString(rel) {
			// The baseline is JSON, which cannot hold such a name.
			skipped = append(skipped, Skipped{Path: rel, Reason: "name is not valid UTF-8"})
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if m != nil && m.Excluded(rel, d.IsDir()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		e := Entry{Path: rel, MTime: info.ModTime().UnixNano()}
		switch mode := info.Mode(); {
		case mode.IsDir():
			e.Type = TypeDir
		case mode&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			e.Type, e.Link = TypeSymlink, filepath.ToSlash(link)
		case mode.IsRegular():
			e.Type, e.Size = TypeFile, info.Size()
			old, ok := prev[rel]
			if ok && old.Type == TypeFile && old.Size == e.Size && old.MTime == e.MTime && old.SHA256 != "" {
				e.SHA256 = old.SHA256
			} else if e.SHA256, err = hashFile(p); err != nil {
				return err
			}
			if runtime.GOOS == "windows" {
				// Windows has no exec bits, so an edit there cannot mean
				// clearing one; keep the bit the file had on the other side.
				e.Exec = ok && old.Type == TypeFile && old.Exec
			} else {
				e.Exec = mode&0o111 != 0
			}
		default:
			skipped = append(skipped, Skipped{Path: rel, Reason: specialReason(mode)})
			return nil
		}
		out[rel] = e
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return out, skipped, nil
}

func specialReason(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeDevice != 0:
		return "device file"
	case mode&fs.ModeNamedPipe != 0:
		return "named pipe"
	case mode&fs.ModeSocket != 0:
		return "socket"
	}
	return "unsupported file type (junction or other reparse point)"
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func Totals(m Manifest) (files int, bytes int64) {
	for _, e := range m {
		if e.Type == TypeFile {
			files++
			bytes += e.Size
		}
	}
	return files, bytes
}
