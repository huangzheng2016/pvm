package diaglog

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const (
	DefaultDaemonMaxBytes  = 8 << 20
	DefaultConsoleMaxBytes = 8 << 20
	DefaultQEMULogMaxBytes = 2 << 20
)

// RotateFile rolls path to path.1 (and older generations up to keep) when
// path exceeds maxBytes. Call it before a process opens the file for append,
// because Windows cannot rename a file held open without delete sharing.
func RotateFile(path string, maxBytes int64, keep int) error {
	if path == "" || maxBytes <= 0 || keep < 1 {
		return nil
	}
	fi, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Size() <= maxBytes {
		return nil
	}
	for i := keep - 1; i >= 1; i-- {
		src := fmt.Sprintf("%s.%d", path, i)
		dst := fmt.Sprintf("%s.%d", path, i+1)
		if _, err := os.Stat(src); err == nil {
			if err := os.Rename(src, dst); err != nil {
				return err
			}
		}
	}
	if err := os.Rename(path, path+".1"); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

// RotateWriter owns its file descriptor so it can rotate safely even while
// the process is running (unlike inherited launchd/stdout descriptors).
type RotateWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	keep     int
	perm     os.FileMode
	f        *os.File
	size     int64
}

func NewRotateWriter(path string, maxBytes int64, keep int, perm os.FileMode) (*RotateWriter, error) {
	w := &RotateWriter{path: path, maxBytes: maxBytes, keep: keep, perm: perm}
	w.mu.Lock()
	err := w.openLocked()
	w.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return w, nil
}

func (w *RotateWriter) openLocked() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, w.perm)
	if err != nil {
		return err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f = f
	w.size = fi.Size()
	return nil
}

func (w *RotateWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		if err := w.openLocked(); err != nil {
			return 0, err
		}
	}
	if w.maxBytes > 0 && w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		_ = w.f.Close()
		w.f = nil
		_ = RotateFile(w.path, 1, w.keep)
		if err := w.openLocked(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *RotateWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}
