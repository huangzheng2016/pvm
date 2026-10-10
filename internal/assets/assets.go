// Package assets unpacks embedded runtime assets (guest agent binaries,
// future QEMU bundles) into ~/.pvm/runtime with SHA256 verification,
// following the platform wails asset model: manifest-driven, zstd
// compressed, versioned, old versions cleaned up.
//
// The embedded FS lives in packed_fs.go and is only compiled in with
// `-tags embedassets`; otherwise Unpack is a no-op.
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

	"github.com/huangzheng2016/pvm/internal/buildinfo"
)

type manifestEntry struct {
	Path   string `json:"path"`   // relative target path under runtime dir
	SHA256 string `json:"sha256"` // of the decompressed payload
	Mode   int32  `json:"mode"`
}

// Unpack extracts all manifest entries into dir, verifying hashes. It is
// safe to call on every startup: files whose hash already matches are
// skipped. Without embedded assets (dev build) it is a no-op.
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
		if err := os.WriteFile(dst, payload, os.FileMode(e.Mode)); err != nil {
			return err
		}
	}
	return nil
}

// CleanStaleVersions removes runtime/<version> dirs that are not the
// current build version. Layout: runtime/assets/<version>/...
func CleanStaleVersions(root string) error {
	base := filepath.Join(root, "assets")
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() && e.Name() != buildinfo.Version {
			if err := os.RemoveAll(filepath.Join(base, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
