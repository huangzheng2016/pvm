package cli

import (
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
)

// clipContent is one snapshot of the host clipboard.
type clipContent struct {
	kind  string // "text", "png" or "files"
	data  []byte
	paths []string
}

func (c clipContent) sum() [32]byte {
	h := sha256.New()
	h.Write([]byte(c.kind + "\x00"))
	h.Write(c.data)
	for _, p := range c.paths {
		fmt.Fprintf(h, "%s\x00", p)
		walkClipFiles([]string{p}, func(name, _ string, d fs.DirEntry) error {
			if fi, err := d.Info(); err == nil {
				fmt.Fprintf(h, "%s %d %d\x00", name, fi.Size(), fi.ModTime().UnixNano())
			}
			return nil
		})
	}
	var sum [32]byte
	h.Sum(sum[:0])
	return sum
}

// clipBridge is what runClipboardBridge may do for a VM.
type clipBridge struct {
	toGuest, toHost bool
	send            func(clipContent) error
	openFiles       func() (io.ReadWriteCloser, error)
	stage           string
}

// sendClipboard writes one snapshot to the guest and waits until the
// guest holds it.
func sendClipboard(open func(kind string) (io.ReadWriteCloser, error), c clipContent) error {
	if c.kind == "files" {
		if err := checkClipFiles(c.paths); err != nil {
			return err
		}
	}
	s, err := open(c.kind)
	if err != nil {
		return err
	}
	defer s.Close()
	if c.kind == "files" {
		err = writeClipFiles(s, c.paths)
	} else {
		_, err = s.Write(c.data)
	}
	if err != nil {
		return err
	}
	if cw, ok := s.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
	}
	_, err = io.Copy(io.Discard, s)
	return err
}
