// assetpack compresses guest binaries into the embed manifest consumed
// by internal/assets. Run before building pvm: it reads packed/*,
// zstd-compresses each to packed/*.zst and rewrites packed/manifest.json.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/klauspost/compress/zstd"
)

type manifestEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Mode   int32  `json:"mode"`
}

func validateInputs(entries []os.DirEntry) error {
	remaining := map[string]bool{
		"agent-linux-" + runtime.GOARCH:   true,
		"gateway-linux-" + runtime.GOARCH: true,
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == "manifest.json" || strings.HasSuffix(name, ".zst") {
			continue
		}
		if !remaining[name] {
			return fmt.Errorf("unexpected asset %q for native architecture %s", name, runtime.GOARCH)
		}
		delete(remaining, name)
	}
	if len(remaining) != 0 {
		return fmt.Errorf("missing native assets for %s: %v", runtime.GOARCH, remaining)
	}
	return nil
}

func main() {
	dir := "internal/assets/packed"
	entries, err := os.ReadDir(dir)
	if err != nil {
		panic(err)
	}
	if err := validateInputs(entries); err != nil {
		panic(err)
	}
	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedBestCompression))
	if err != nil {
		panic(err)
	}
	var manifest []manifestEntry
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == "manifest.json" || strings.HasSuffix(name, ".zst") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			panic(err)
		}
		sum := sha256.Sum256(raw)
		out := enc.EncodeAll(raw, nil)
		if err := os.WriteFile(filepath.Join(dir, name+".zst"), out, 0o644); err != nil {
			panic(err)
		}
		var mode int32 = 0o755
		manifest = append(manifest, manifestEntry{
			Path:   name,
			SHA256: hex.EncodeToString(sum[:]),
			Mode:   mode,
		})
		fmt.Printf("packed %-28s %8d -> %8d bytes\n", name, len(raw), len(out))
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), data, 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("manifest.json: %d entries\n", len(manifest))
}
