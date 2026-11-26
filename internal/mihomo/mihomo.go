// Package mihomo downloads and verifies the pinned Mihomo proxy core as a
// standalone GPL-3.0 executable. It is never linked into pvm binaries.
package mihomo

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/huangzheng2016/pvm/internal/fsutil"
)

const Version = "v1.19.34"

type Asset struct {
	URL    string
	SHA256 string // of the .gz archive
}

var pinned = map[string]Asset{
	"amd64": {
		URL:    "https://github.com/MetaCubeX/mihomo/releases/download/v1.19.34/mihomo-linux-amd64-v1.19.34.gz",
		SHA256: "223fab877e5795ce780ba39902e5f0b1113c1256ae244a7160f1165e017e9ccc",
	},
	"amd64-compatible": {
		URL:    "https://github.com/MetaCubeX/mihomo/releases/download/v1.19.34/mihomo-linux-amd64-compatible-v1.19.34.gz",
		SHA256: "2763f5b414527f7baf6dc57e3f82f3ab561eedf34bfd7c2f2cde1f7e592efe06",
	},
	"arm64": {
		URL:    "https://github.com/MetaCubeX/mihomo/releases/download/v1.19.34/mihomo-linux-arm64-v1.19.34.gz",
		SHA256: "daaf78a6240a282cdfd75bcbf98fe321f6bd52765cc29e79f344191198f0b50f",
	},
}

const licenseNotice = "Mihomo is free software licensed under the GNU General Public License v3.0 (GPL-3.0).\nSource code and license: https://github.com/MetaCubeX/mihomo\nDistributed by pvm as an independent executable; it is not linked into pvm.\n"

var httpClient = &http.Client{Timeout: 5 * time.Minute}

func BinaryName(arch string) string { return "mihomo-linux-" + arch }

// EnsureBinary returns a verified local Mihomo binary for the target Linux arch.
func EnsureBinary(runtimeDir, arch string, progress io.Writer) (string, error) {
	asset, ok := pinned[arch]
	if !ok {
		return "", fmt.Errorf("unsupported Mihomo arch %q", arch)
	}
	dir := filepath.Join(runtimeDir, "mihomo", Version)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	final := filepath.Join(dir, BinaryName(arch))
	if fi, err := os.Stat(final); err == nil && fi.Size() > 0 && fi.Mode()&0o111 != 0 {
		return final, nil
	}
	lock, err := fsutil.TryLock(final + ".lock")
	if err != nil {
		return "", fmt.Errorf("mihomo download in progress elsewhere: %w", err)
	}
	defer lock.Unlock()

	gzPath := final + ".gz"
	if !fileMatchesSHA256(gzPath, asset.SHA256) {
		if progress != nil {
			fmt.Fprintf(progress, "downloading Mihomo %s (%s)...\n", Version, arch)
		}
		req, err := http.NewRequest(http.MethodGet, asset.URL, nil)
		if err != nil {
			return "", err
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return "", fmt.Errorf("download Mihomo: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("download Mihomo: %s", resp.Status)
		}
		tmp, err := os.CreateTemp(dir, ".mihomo-*.part")
		if err != nil {
			return "", err
		}
		tmpName := tmp.Name()
		defer os.Remove(tmpName)
		h := sha256.New()
		if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
			tmp.Close()
			return "", err
		}
		if err := tmp.Close(); err != nil {
			return "", err
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != asset.SHA256 {
			return "", fmt.Errorf("Mihomo archive checksum mismatch: got %s, want %s", got, asset.SHA256)
		}
		if err := os.Rename(tmpName, gzPath); err != nil {
			return "", err
		}
	}

	f, err := os.Open(gzPath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	// Bound decompression to 256 MiB.
	tmpBin, err := os.CreateTemp(dir, ".mihomo-bin-*")
	if err != nil {
		return "", err
	}
	tmpBinName := tmpBin.Name()
	defer os.Remove(tmpBinName)
	if _, err := io.Copy(tmpBin, io.LimitReader(zr, 256<<20)); err != nil {
		tmpBin.Close()
		return "", err
	}
	if err := tmpBin.Sync(); err != nil {
		tmpBin.Close()
		return "", err
	}
	if err := tmpBin.Close(); err != nil {
		return "", err
	}
	if err := os.Chmod(tmpBinName, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmpBinName, final); err != nil {
		return "", err
	}
	_ = fsutil.AtomicWrite(filepath.Join(dir, "LICENSE.txt"), []byte(licenseNotice), 0o644)
	return final, nil
}

func fileMatchesSHA256(path, want string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == want
}
