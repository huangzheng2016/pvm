package qemu

import (
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
)

// SnapshotPipeOut is the hidden helper used by QMP exec: migration URIs.
func SnapshotPipeOut(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := snapCompress(f, os.Stdin); err != nil {
		return err
	}
	return f.Sync()
}

func SnapshotPipeIn(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return snapDecompress(os.Stdout, f)
}

func snapCompress(dst io.Writer, src io.Reader) error {
	w, err := zstd.NewWriter(dst, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, src); err != nil {
		w.Close()
		return fmt.Errorf("pipe-out copy: %w", err)
	}
	return w.Close()
}

func snapDecompress(dst io.Writer, src io.Reader) error {
	r, err := zstd.NewReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	_, err = io.Copy(dst, r)
	return err
}
