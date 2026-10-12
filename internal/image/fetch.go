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
	"sync"
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
		dir, file, err := debianImagePaths(version, qarch)
		if err != nil {
			return nil, err
		}
		base := "https://cloud.debian.org/images/cloud/" + dir
		return &Ref{Distro: distro, Version: version, Arch: native.Arch,
			URL: base + "/" + file, SumsURL: base + "/SHA512SUMS", SumFile: file, SumKind: "sha512"}, nil
	}
	return nil, fmt.Errorf("unknown distro %q (supported: ubuntu, debian)", distro)
}

// Debian 14 pre-release uses daily paths and a -daily filename suffix.
func debianImagePaths(v, qarch string) (dir, file string, err error) {
	switch v {
	case "12":
		return "bookworm/latest", "debian-12-generic-" + qarch + ".qcow2", nil
	case "13":
		return "trixie/latest", "debian-13-generic-" + qarch + ".qcow2", nil
	case "14":
		return "forky/daily/latest", "debian-14-generic-" + qarch + "-daily.qcow2", nil
	}
	return "", "", fmt.Errorf("unsupported debian version %q (supported: 12, 13, 14)", v)
}

func (r *Ref) FileName() string {
	return fmt.Sprintf("%s-%s-%s.qcow2", r.Distro, r.Version, r.Arch)
}

var httpClient = &http.Client{
	// No overall timeout: multi-GB downloads must not be cut mid-flight.
	// Stalls are bounded separately (response header + per-read idle).
	Timeout:   0,
	Transport: defaultTransportWithHeaderTimeout(),
}

func defaultTransportWithHeaderTimeout() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 30 * time.Second
	return t
}

// stallReadCloser errors the read loop when the server goes silent for d;
// Fetch keeps the .part file, so the next invocation resumes where it
// stopped instead of hanging forever.
type stallReadCloser struct {
	rc       io.ReadCloser
	d        time.Duration
	progress chan struct{}
	done     chan struct{}
	once     sync.Once
}

func withStallTimeout(rc io.ReadCloser, d time.Duration) io.ReadCloser {
	s := &stallReadCloser{rc: rc, d: d, progress: make(chan struct{}, 1), done: make(chan struct{})}
	go s.watch()
	return s
}

func (s *stallReadCloser) Read(p []byte) (int, error) {
	n, err := s.rc.Read(p)
	if n > 0 {
		select {
		case s.progress <- struct{}{}:
		default:
		}
	}
	return n, err
}

func (s *stallReadCloser) Close() error {
	s.once.Do(func() { close(s.done) })
	return s.rc.Close()
}

func (s *stallReadCloser) watch() {
	t := time.NewTimer(s.d)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-s.progress:
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
			t.Reset(s.d)
		case <-t.C:
			s.rc.Close() // unblocks Read with an error
			return
		}
	}
}

// Fetch downloads the image into dir with resume, progress and checksum
// verification. Returns the final path. A verified cached file is reused
// unless force is set.
func Fetch(r *Ref, dir string, progress io.Writer, force ...bool) (string, error) {
	final := filepath.Join(dir, r.FileName())
	part := final + ".part"

	if fi, err := os.Stat(final); err == nil && fi.Size() > 0 && (len(force) == 0 || !force[0]) {
		if err := verifyChecksum(r, final); err == nil {
			if progress != nil {
				fmt.Fprintf(progress, "cached: %s (checksum verified)\n", final)
			}
			return final, nil
		} else {
			os.Remove(final)
		}
	}

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
	body := withStallTimeout(resp.Body, 60*time.Second)
	defer body.Close()
	for {
		n, err := body.Read(buf)
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
			return "", fmt.Errorf("download interrupted at %d/%d MB (%v); partial file kept, rerun to resume", have>>20, total>>20, err)
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
