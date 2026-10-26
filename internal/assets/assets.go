// Package assets embeds guest binaries only in host builds; without the
// embedassets tag, Unpack is a no-op.
package assets

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/klauspost/compress/zstd"

	"github.com/huangzheng2016/pvm/internal/fsutil"
	"github.com/huangzheng2016/pvm/internal/native"
)

type manifestEntry struct {
	Path   string `json:"path"`   // relative target path under runtime dir
	SHA256 string `json:"sha256"` // of the decompressed payload
	Mode   int32  `json:"mode"`
}

func Unpack(dir string) error {
	data, err := packed.ReadFile("packed/manifest.json")
	if err != nil {
		if err == fs.ErrNotExist {
			return nil
		}
		return fmt.Errorf("read manifest: %w", err)
	}
	var entries []manifestEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return err
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return err
	}
	defer dec.Close()

	for _, e := range entries {
		if e.Path != native.GuestBinary("agent") && e.Path != native.GuestBinary("gateway") {
			return fmt.Errorf("unsupported embedded asset %q (only native agent/gateway binaries are allowed)", e.Path)
		}
		raw, err := packed.ReadFile("packed/" + e.Path + ".zst")
		if err != nil {
			return fmt.Errorf("read packed %s: %w", e.Path, err)
		}
		payload, err := dec.DecodeAll(raw, nil)
		if err != nil {
			return fmt.Errorf("decompress %s: %w", e.Path, err)
		}
		sum := sha256.Sum256(payload)
		if hex.EncodeToString(sum[:]) != e.SHA256 {
			return fmt.Errorf("sha256 mismatch for %s", e.Path)
		}
		dst := filepath.Join(dir, e.Path)
		if existing, err := os.ReadFile(dst); err == nil {
			if hex.EncodeToString(sum[:]) == sha256Hex(existing) {
				continue
			}
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := fsutil.AtomicWrite(dst, payload, os.FileMode(e.Mode)); err != nil {
			return err
		}
	}
	return nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
