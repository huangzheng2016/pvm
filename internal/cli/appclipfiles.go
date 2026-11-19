package cli

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Files copied through the clipboard, either way, go as a tar of folders
// and regular files, bounded like the agent's maxClipFiles.
const (
	maxClipFiles   = 256 << 20
	maxClipEntries = 20000
)

// winReserved are names Windows does not allow for files.
var winReserved = map[string]bool{"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true}

// clipFileName checks a name from the VM's tar and returns it cleaned, or
// "" when it could leave the folder or is not a valid Windows name.
func clipFileName(name string) string {
	if strings.ContainsAny(name, `\:`) {
		return ""
	}
	p := path.Clean(strings.TrimPrefix(name, "./"))
	if p == "." || p == ".." || path.IsAbs(p) || strings.HasPrefix(p, "../") {
		return ""
	}
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." || c == ".." || strings.ContainsAny(c, `<>"|?*`) || strings.HasSuffix(c, ".") || strings.HasSuffix(c, " ") {
			return ""
		}
		for _, r := range c {
			if r < 32 {
				return ""
			}
		}
		stem, _, _ := strings.Cut(c, ".")
		if winReserved[strings.ToUpper(stem)] {
			return ""
		}
	}
	return p
}

// extractClipFiles unpacks the VM's tar into dir and returns its top
// level entries. It keeps only folders and regular files with valid
// names, reports the names it skips to skipped, and fails past
// maxClipFiles bytes or maxClipEntries entries.
func extractClipFiles(r io.Reader, dir string, skipped io.Writer) ([]string, error) {
	tr := tar.NewReader(r)
	var tops []string
	seen := map[string]bool{}
	var total int64
	for n := 0; ; n++ {
		hdr, err := tr.Next()
		if err == io.EOF {
			return tops, nil
		}
		if err != nil {
			return nil, err
		}
		if n >= maxClipEntries {
			return nil, fmt.Errorf("more than %d files", maxClipEntries)
		}
		if hdr.Typeflag != tar.TypeDir && hdr.Typeflag != tar.TypeReg {
			continue
		}
		name := clipFileName(hdr.Name)
		if name == "" {
			fmt.Fprintf(skipped, "skipped %q: not a valid file name on this host\n", hdr.Name)
			continue
		}
		dst := filepath.Join(dir, filepath.FromSlash(name))
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(dst, 0o700); err != nil {
				return nil, err
			}
		} else {
			if total += hdr.Size; total > maxClipFiles {
				return nil, fmt.Errorf("the files are larger than %d MB; use a workspace", maxClipFiles>>20)
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return nil, err
			}
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return nil, err
			}
			_, err = io.CopyN(f, tr, hdr.Size)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return nil, err
			}
			os.Chtimes(dst, hdr.ModTime, hdr.ModTime)
		}
		top, _, _ := strings.Cut(name, "/")
		if !seen[top] {
			seen[top] = true
			tops = append(tops, filepath.Join(dir, top))
		}
	}
}

// walkClipFiles calls fn for every folder and regular file under paths,
// with its name in the tar; links and other special files are left out.
func walkClipFiles(paths []string, fn func(name, p string, d fs.DirEntry) error) error {
	for _, top := range paths {
		base := filepath.Base(top)
		err := filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && !d.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(top, p)
			if err != nil {
				return err
			}
			name := base
			if rel != "." {
				name = base + "/" + filepath.ToSlash(rel)
			}
			return fn(name, p, d)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// checkClipFiles fails when paths hold more than the clipboard carries.
func checkClipFiles(paths []string) error {
	var total int64
	n := 0
	return walkClipFiles(paths, func(_, _ string, d fs.DirEntry) error {
		if n++; n > maxClipEntries {
			return fmt.Errorf("more than %d files", maxClipEntries)
		}
		if d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if total += fi.Size(); total > maxClipFiles {
			return fmt.Errorf("the copied files are larger than %d MB; use a workspace", maxClipFiles>>20)
		}
		return nil
	})
}

// writeClipFiles writes paths to w as a tar, as the agent's
// clipFilesScript unpacks it.
func writeClipFiles(w io.Writer, paths []string) error {
	tw := tar.NewWriter(w)
	err := walkClipFiles(paths, func(name, p string, d fs.DirEntry) error {
		fi, err := d.Info()
		if err != nil {
			return err
		}
		hdr := &tar.Header{Name: name, ModTime: fi.ModTime(), Mode: 0o644, Typeflag: tar.TypeReg, Size: fi.Size()}
		if d.IsDir() {
			hdr.Name, hdr.Mode, hdr.Typeflag, hdr.Size = name+"/", 0o755, tar.TypeDir, 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		// a file that grew since the header would break the tar
		_, err = io.CopyN(tw, f, fi.Size())
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}
