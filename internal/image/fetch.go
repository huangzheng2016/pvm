package image

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/huangzheng2016/pvm/internal/fsutil"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/huangzheng2016/pvm/internal/native"
)

type Ref struct {
	Distro         string
	Version        string
	Arch           string // GOARCH
	URL            string
	SumsURL        string
	SumFile        string // filename inside the sums file
	SumKind        string // sha256 | sha512
	SignatureURL   string
	TrustedKeyring string // explicit local publisher trust anchor; empty keeps HTTPS checksum verification
}

var ubuntuVersionRE = regexp.MustCompile(`^[0-9]{2}\.[0-9]{2}$`)

func Resolve(spec string) (*Ref, error) {
	distro, version, ok := strings.Cut(spec, ":")
	if !ok {
		return nil, fmt.Errorf("image spec must be distro:version, got %q", spec)
	}
	qarch := native.Arch
	switch distro {
	case "ubuntu":
		if !ubuntuVersionRE.MatchString(version) {
			return nil, fmt.Errorf("invalid ubuntu version %q (expected e.g. 24.04)", version)
		}
		base := fmt.Sprintf("https://cloud-images.ubuntu.com/releases/%s/release", version)
		file := fmt.Sprintf("ubuntu-%s-server-cloudimg-%s.img", version, qarch)
		return &Ref{Distro: distro, Version: version, Arch: native.Arch,
			URL: base + "/" + file, SumsURL: base + "/SHA256SUMS", SignatureURL: base + "/SHA256SUMS.gpg", SumFile: file, SumKind: "sha256"}, nil
	case "debian":
		dir, file, err := debianImagePaths(version, qarch)
		if err != nil {
			return nil, err
		}
		base := "https://cloud.debian.org/images/cloud/" + dir
		return &Ref{Distro: distro, Version: version, Arch: native.Arch,
			URL: base + "/" + file, SumsURL: base + "/SHA512SUMS", SignatureURL: base + "/SHA512SUMS.sign", SumFile: file, SumKind: "sha512"}, nil
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

const fetchMaxAttempts = 3

var fetchRetrySleep = func(attempt int) { time.Sleep(time.Duration(attempt) * time.Second) }

// Fetch downloads the image into dir with resume, progress and checksum
// verification. Returns the final path. A verified cached file is reused
// unless force is set.
func Fetch(r *Ref, dir string, progress io.Writer, force ...bool) (string, error) {
	if _, _, err := signatureConfig(r); err != nil {
		return "", err
	}
	lock, err := fsutil.TryLock(filepath.Join(dir, r.FileName()+".lock"))
	if err != nil {
		return "", fmt.Errorf("image is in use by another download or build; retry: %w", err)
	}
	defer lock.Unlock()
	return fetch(r, dir, progress, true, force...)
}

func fetch(r *Ref, dir string, progress io.Writer, retry bool, force ...bool) (string, error) {
	final := filepath.Join(dir, r.FileName())
	part := final + ".part"
	checkCached := false
	if fi, err := os.Stat(final); err == nil && fi.Size() > 0 && (len(force) == 0 || !force[0]) {
		checkCached = true
	}

	want, err := expectedSum(r, dir, checkCached)
	if err != nil {
		return "", err
	}
	if checkCached {
		if err := verifyFileSum(r, final, want); err == nil {
			if progress != nil {
				fmt.Fprintf(progress, "cached: %s (checksum verified)\n", final)
			}
			return final, nil
		} else if isChecksumMismatch(err) {
			os.Remove(final)
		} else {
			return "", fmt.Errorf("verify cached image %s: %w", final, err)
		}
	}

	attempts := 1
	if retry {
		attempts = fetchMaxAttempts
	}
	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if progress != nil {
				fmt.Fprintf(progress, "\n  %s: retrying (%d/%d): %v\n", r.FileName(), attempt+1, attempts, lastErr)
			}
			fetchRetrySleep(attempt)
		}
		done, err := downloadPart(r, part, want, progress)
		if err != nil {
			lastErr = err
			continue
		}
		if done {
			return final, os.Rename(part, final)
		}
	}
	return "", fmt.Errorf("download %s failed after %d attempt(s); partial file kept, rerun to resume: %w", r.URL, attempts, lastErr)
}

// downloadPart advances part toward want in one HTTP transfer. A false done
// with nil error asks the caller to restart from offset zero immediately.
func downloadPart(r *Ref, part, want string, progress io.Writer) (bool, error) {
	sumMarker := part + ".sum"
	if data, err := os.ReadFile(sumMarker); err == nil && strings.TrimSpace(string(data)) != want {
		// The upstream release moved on: splicing the new tail onto the old
		// prefix would download the whole file just to fail the checksum.
		os.Remove(part)
	}
	if err := fsutil.AtomicWrite(sumMarker, []byte(want+"\n"), 0o644); err != nil {
		return false, err
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
			if err := verifyFileSum(r, part, want); err == nil {
				os.Remove(sumMarker)
				return true, nil
			} else if !isChecksumMismatch(err) {
				return false, err
			}
			os.Remove(part)
			have = 0
		} else if resp != nil {
			resp.Body.Close()
		}
	}
	req, err := http.NewRequest("GET", r.URL, nil)
	if err != nil {
		return false, err
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && have > 0 {
		if err := verifyFileSum(r, part, want); err == nil {
			os.Remove(sumMarker)
			return true, nil
		}
		os.Remove(part)
		return false, nil
	}
	if resp.StatusCode >= 400 {
		return false, fmt.Errorf("download %s: %s", r.URL, resp.Status)
	}
	var total int64 = -1
	if have > 0 {
		if resp.StatusCode == http.StatusPartialContent {
			start, end, size, err := parseContentRange(resp.Header.Get("Content-Range"))
			if err != nil || start != have {
				os.Remove(part)
				return false, fmt.Errorf("invalid Content-Range %q for offset %d; partial file reset", resp.Header.Get("Content-Range"), have)
			}
			if size > 0 && end >= size {
				os.Remove(part)
				return false, fmt.Errorf("invalid Content-Range %q end %d >= total %d; partial file reset", resp.Header.Get("Content-Range"), end, size)
			}
			total = size
		} else {
			have = 0 // server ignored the range; start over
		}
	}
	if total < 0 && resp.ContentLength >= 0 {
		total = resp.ContentLength + have
	}

	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return false, err
	}
	if have > 0 {
		if _, err := f.Seek(have, io.SeekStart); err != nil {
			f.Close()
			return false, err
		}
	} else {
		if err := f.Truncate(0); err != nil {
			f.Close()
			return false, err
		}
	}

	buf := make([]byte, 1<<20)
	last := time.Now()
	body := withStallTimeout(resp.Body, 60*time.Second)
	defer body.Close()
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if total > 0 && have+int64(n) > total {
				f.Close()
				os.Remove(part)
				return false, fmt.Errorf("server sent more than declared size %d; partial file reset", total)
			}
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				return false, werr
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
			_ = f.Sync()
			f.Close()
			return false, fmt.Errorf("download interrupted at %d/%d MB: %w", have>>20, total>>20, err)
		}
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	if progress != nil {
		fmt.Fprintf(progress, "\n")
	}

	if err := verifyFileSum(r, part, want); err != nil {
		if isChecksumMismatch(err) {
			os.Remove(part)
		}
		return false, err
	}
	os.Remove(sumMarker)
	return true, nil
}

func parseContentRange(v string) (start, end, total int64, err error) {
	unit, rest, ok := strings.Cut(strings.TrimSpace(v), " ")
	if !ok || !strings.EqualFold(unit, "bytes") {
		return 0, 0, 0, fmt.Errorf("malformed Content-Range %q", v)
	}
	rangePart, totalPart, ok := strings.Cut(rest, "/")
	if !ok {
		return 0, 0, 0, fmt.Errorf("malformed Content-Range %q", v)
	}
	startPart, endPart, ok := strings.Cut(rangePart, "-")
	if !ok {
		return 0, 0, 0, fmt.Errorf("malformed Content-Range %q", v)
	}
	start, err1 := strconv.ParseInt(startPart, 10, 64)
	end, err2 := strconv.ParseInt(endPart, 10, 64)
	if err1 != nil || err2 != nil || start < 0 || end < start {
		return 0, 0, 0, fmt.Errorf("malformed Content-Range %q", v)
	}
	if totalPart == "*" {
		total = -1
	} else {
		total, err = strconv.ParseInt(totalPart, 10, 64)
		if err != nil || total <= 0 {
			return 0, 0, 0, fmt.Errorf("malformed Content-Range %q", v)
		}
	}
	return start, end, total, nil
}

type checksumMismatchError struct {
	file      string
	got, want string
}

func (e *checksumMismatchError) Error() string {
	return fmt.Sprintf("checksum mismatch for %s: got %s want %s", e.file, e.got, e.want)
}

func isChecksumMismatch(err error) bool {
	var target *checksumMismatchError
	return errors.As(err, &target)
}

func fetchSums(r *Ref) ([]byte, error) {
	resp, err := httpClient.Get(r.SumsURL)
	if err != nil {
		return nil, fmt.Errorf("fetch sums: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("fetch sums %s: %s", r.SumsURL, resp.Status)
	}
	body := withStallTimeout(resp.Body, 60*time.Second)
	defer body.Close()
	const maxSumsBytes = 8 << 20
	sums, err := io.ReadAll(io.LimitReader(body, maxSumsBytes+1))
	if err != nil {
		return nil, err
	}
	if len(sums) > maxSumsBytes {
		return nil, fmt.Errorf("checksum list exceeds %d bytes", maxSumsBytes)
	}
	if err := verifyChecksumSignature(r, sums); err != nil {
		return nil, err
	}
	return sums, nil
}

// expectedSum resolves the pinned digest from the signed/HTTPS checksum list,
// caching the verified list so an already verified base image can be reused
// when the checksum endpoint is temporarily unreachable. Explicit keyring
// verification never falls back to an unverified cached signature.
func expectedSum(r *Ref, dir string, allowCached bool) (string, error) {
	sums, err := fetchSums(r)
	if err == nil {
		if werr := fsutil.AtomicWrite(filepath.Join(dir, r.FileName()+".sums"), sums, 0o644); werr != nil {
			return "", werr
		}
		return parseSumEntry(r, sums)
	}
	if allowCached && r.TrustedKeyring == "" {
		if cached, cerr := os.ReadFile(filepath.Join(dir, r.FileName()+".sums")); cerr == nil {
			if want, perr := parseSumEntry(r, cached); perr == nil {
				return want, nil
			}
		}
	}
	return "", err
}

func parseSumEntry(r *Ref, sums []byte) (string, error) {
	for line := range strings.Lines(string(sums)) {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == r.SumFile {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("no checksum entry for %s in %s", r.SumFile, r.SumsURL)
}

func verifyChecksum(r *Ref, file string) error {
	sums, err := fetchSums(r)
	if err != nil {
		return err
	}
	want, err := parseSumEntry(r, sums)
	if err != nil {
		return err
	}
	return verifyFileSum(r, file, want)
}

func verifyFileSum(r *Ref, file, want string) error {
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
		return &checksumMismatchError{file: r.SumFile, got: got, want: want}
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
