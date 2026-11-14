package workspace

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// WriteTar streams entries from root as a PAX archive with names
// prefix+path. Files are reopened through an os.Root, and any file whose
// size or hash no longer matches its entry fails the transfer.
func WriteTar(w io.Writer, root string, entries []Entry, prefix string) error {
	r, err := os.OpenRoot(root)
	if err != nil {
		return err
	}
	defer r.Close()
	tw := tar.NewWriter(w)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:    prefix + e.Path,
			ModTime: time.Unix(0, e.MTime),
			Mode:    0o644,
			Format:  tar.FormatPAX,
		}
		switch e.Type {
		case TypeDir:
			hdr.Typeflag, hdr.Name, hdr.Mode = tar.TypeDir, hdr.Name+"/", 0o755
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
		case TypeSymlink:
			hdr.Typeflag, hdr.Linkname, hdr.Mode = tar.TypeSymlink, e.Link, 0o777
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
		case TypeFile:
			hdr.Typeflag, hdr.Size = tar.TypeReg, e.Size
			if e.Exec {
				hdr.Mode = 0o755
			}
			if err := writeTarFile(tw, r, hdr, e); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s: unsupported entry type %q", e.Path, e.Type)
		}
	}
	return tw.Close()
}

func writeTarFile(tw *tar.Writer, r *os.Root, hdr *tar.Header, e Entry) error {
	changed := fmt.Errorf("%s changed during transfer; rerun the command", e.Path)
	f, err := r.Open(filepath.FromSlash(e.Path))
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || fi.Size() != e.Size {
		return changed
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	h := sha256.New()
	if _, err := io.CopyN(io.MultiWriter(tw, h), f, e.Size); err == io.EOF {
		return changed
	} else if err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != e.SHA256 {
		return changed
	}
	return nil
}
