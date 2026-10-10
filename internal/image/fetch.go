// Package image fetches and catalogs golden base images.
package image

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/huangzheng2016/pvm/internal/native"
)

type Ref struct {
	Distro  string
	Version string
	Arch    string // GOARCH
	URL     string
	SumsURL string
	SumFile string // filename inside the sums file
	SumKind string // sha256 | sha512
}

// Resolve maps "ubuntu:24.04" / "debian:13" to native download URLs.
func Resolve(spec string) (*Ref, error) {
	distro, version, ok := strings.Cut(spec, ":")
	if !ok {
		return nil, fmt.Errorf("image spec must be distro:version, got %q", spec)
	}
	qarch := native.Arch
	switch distro {
	case "ubuntu":
		base := fmt.Sprintf("https://cloud-images.ubuntu.com/releases/%s/release", version)
		file := fmt.Sprintf("ubuntu-%s-server-cloudimg-%s.img", version, qarch)
		return &Ref{Distro: distro, Version: version, Arch: native.Arch,
			URL: base + "/" + file, SumsURL: base + "/SHA256SUMS", SumFile: file, SumKind: "sha256"}, nil
	case "debian":
		base := fmt.Sprintf("https://cloud.debian.org/images/cloud/%s/latest", debianCodename(version))
		file := fmt.Sprintf("debian-%s-generic-%s.qcow2", version, qarch)
		return &Ref{Distro: distro, Version: version, Arch: native.Arch,
			URL: base + "/" + file, SumsURL: base + "/SHA512SUMS", SumFile: file, SumKind: "sha512"}, nil
	case "alpine":
		if qarch == "arm64" {
			qarch = "aarch64" // Alpine names its arm64 images aarch64.
		}
		var file string
		switch version {
		case "3.20":
			file = fmt.Sprintf("nocloud_alpine-3.20.9-%s-uefi-cloudinit-r0.qcow2", qarch)
		case "3.21":
			file = fmt.Sprintf("alpine-3.21.8-%s-uefi-cloudinit-r0.qcow2", qarch)
		default:
			return nil, fmt.Errorf("unsupported alpine version %q (supported: 3.20, 3.21)", version)
		}
		base := "https://dl-cdn.alpinelinux.org/alpine/v" + version + "/releases/cloud"
		return &Ref{Distro: distro, Version: version, Arch: native.Arch,
			URL: base + "/" + file, SumsURL: base + "/" + file + ".sha512", SumFile: file, SumKind: "sha512"}, nil
	}
	return nil, fmt.Errorf("unknown distro %q (supported: ubuntu, debian, alpine)", distro)
}

func debianCodename(v string) string {
	switch v {
	case "12":
		return "bookworm"
	case "13":
		return "trixie"
	default:
		return "trixie"
	}
}

// FileName is the canonical local file name for a fetched image.
func (r *Ref) FileName() string {
	return fmt.Sprintf("%s-%s-%s.qcow2", r.Distro, r.Version, r.Arch)
}

var httpClient = &http.Client{Timeout: 0} // no global timeout; resume handles stalls manually

// Fetch downloads the image into dir with resume, progress and checksum
// verification. Returns the final path.
func Fetch(r *Ref, dir string, progress io.Writer) (string, error) {
	final := filepath.Join(dir, r.FileName())
	part := final + ".part"

	var have int64
	if fi, err := os.Stat(part); err == nil {
		have = fi.Size()
	}
	if have > 0 {
		// A fully downloaded .part may be finalized without issuing another
		// range request (some servers return 416 for an exact-size range).
		if resp, err := httpClient.Head(r.URL); err == nil && resp.ContentLength == have {
			resp.Body.Close()
			if err := verifyChecksum(r, part); err != nil {
				return "", err
			}
			return final, os.Rename(part, final)
		} else if resp != nil {
			resp.Body.Close()
		}
	}
	req, err := http.NewRequest("GET", r.URL, nil)
	if err != nil {
		return "", err
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if have > 0 && resp.StatusCode != http.StatusPartialContent {
		have = 0 // server ignored the range; start over
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("download %s: %s", r.URL, resp.Status)
	}

	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	if have > 0 {
		if _, err := f.Seek(have, io.SeekStart); err != nil {
			f.Close()
			return "", err
		}
	} else {
		f.Truncate(0)
	}

	total := resp.ContentLength + have
	buf := make([]byte, 1<<20)
	last := time.Now()
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				return "", werr
			}
			have += int64(n)
			if progress != nil && total > 0 && time.Since(last) > 500*time.Millisecond {
				fmt.Fprintf(progress, "\r  %s: %.1f%% (%d/%d MB)", r.FileName(), float64(have)*100/float64(total), have>>20, total>>20)
				last = time.Now()
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			f.Close()
			return "", err
		}
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if progress != nil {
		fmt.Fprintf(progress, "\n")
	}

	if err := verifyChecksum(r, part); err != nil {
		return "", err
	}
	return final, os.Rename(part, final)
}

func verifyChecksum(r *Ref, file string) error {
	resp, err := httpClient.Get(r.SumsURL)
	if err != nil {
		return fmt.Errorf("fetch sums: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("fetch sums %s: %s", r.SumsURL, resp.Status)
	}
	sums, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var want string
	for line := range strings.Lines(string(sums)) {
		fields := strings.Fields(line)
		if len(fields) == 1 {
			// Alpine publishes a bare hash with no filename column.
			want = fields[0]
			break
		}
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == r.SumFile {
			want = fields[0]
			break
		}
	}
	if want == "" {
		return fmt.Errorf("no checksum entry for %s in %s", r.SumFile, r.SumsURL)
	}

	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	var h hash.Hash
	if r.SumKind == "sha512" {
		h = sha512.New()
	} else {
		h = sha256.New()
	}
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("checksum mismatch for %s: got %s want %s", r.SumFile, got, want)
	}
	return nil
}

type LocalImage struct {
	Name string
	Path string
	Size int64
}

// List returns fetched images in dir.
func List(dir string) ([]LocalImage, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []LocalImage
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".qcow2") {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, LocalImage{Name: e.Name(), Path: filepath.Join(dir, e.Name()), Size: fi.Size()})
	}
	return out, nil
}
