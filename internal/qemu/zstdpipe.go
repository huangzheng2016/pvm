package qemu

import (
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
)

// SnapshotPipeOut compresses stdin into path with zstd. It runs as the
// hidden `pvm __snapshot-pipe-out <path>` helper inside a QMP exec: URI.
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

// SnapshotPipeIn decompresses path to stdout (the -incoming counterpart).
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
