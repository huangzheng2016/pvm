package image

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func testSumServer(t *testing.T, payload []byte) *httptest.Server {
	t.Helper()
	sum := sha256.Sum256(payload)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "%s  ubuntu-24.04-arm64.qcow2\n", hex.EncodeToString(sum[:]))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testFetchRef(imgURL, sumsURL string) *Ref {
	return &Ref{Distro: "ubuntu", Version: "24.04", Arch: "arm64", URL: imgURL, SumsURL: sumsURL, SumFile: "ubuntu-24.04-arm64.qcow2", SumKind: "sha256"}
}

func stubFetchSleep(t *testing.T) {
	t.Helper()
	old := fetchRetrySleep
	fetchRetrySleep = func(int) {}
	t.Cleanup(func() { fetchRetrySleep = old })
}

func TestFetchValidatesContentRangeStart(t *testing.T) {
	stubFetchSleep(t)
	payload := []byte("0123456789abcdef")
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			return
		}
		n := gets.Add(1)
		if r.Header.Get("Range") != "" && n == 1 {
			// Broken mirror ignores the requested offset and returns the tail from 0.
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(payload)-1, len(payload)))
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(payload)
			return
		}
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	sums := testSumServer(t, payload)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ubuntu-24.04-arm64.qcow2.part"), payload[:8], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Fetch(testFetchRef(srv.URL, sums.URL), dir, io.Discard); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "ubuntu-24.04-arm64.qcow2"))
	if err != nil || string(got) != string(payload) {
		t.Fatalf("final = %q, %v", got, err)
	}
	if gets.Load() != 2 {
		t.Fatalf("GETs = %d, want 2 (mismatched 206 then clean GET)", gets.Load())
	}
}

func TestFetchDiscardsStalePartWhenDigestChanges(t *testing.T) {
	stubFetchSleep(t)
	payload := []byte("new-release-payload")
	var sawRange atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			return
		}
		if r.Header.Get("Range") != "" {
			sawRange.Store(true)
		}
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	sums := testSumServer(t, payload)
	dir := t.TempDir()
	part := filepath.Join(dir, "ubuntu-24.04-arm64.qcow2.part")
	if err := os.WriteFile(part, []byte("old-release-prefix"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(part+".sum", []byte(strings.Repeat("0", 64)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Fetch(testFetchRef(srv.URL, sums.URL), dir, io.Discard); err != nil {
		t.Fatal(err)
	}
	if sawRange.Load() {
		t.Fatal("stale .part was resumed with a Range request after digest changed")
	}
}

func TestFetchRecoversFrom416(t *testing.T) {
	stubFetchSleep(t)
	payload := []byte("short-image")
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			return
		}
		gets.Add(1)
		if rng := r.Header.Get("Range"); rng != "" {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(payload)))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	sums := testSumServer(t, payload)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ubuntu-24.04-arm64.qcow2.part"), []byte("oversized-stale-partial-file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Fetch(testFetchRef(srv.URL, sums.URL), dir, io.Discard); err != nil {
		t.Fatal(err)
	}
	if gets.Load() != 2 {
		t.Fatalf("GETs = %d, want 2 (416 then full GET)", gets.Load())
	}
}

func TestFetchRetriesInterruptedTransfer(t *testing.T) {
	stubFetchSleep(t)
	payload := []byte(strings.Repeat("pvm-resume-", 4096))
	half := len(payload) / 2
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			return
		}
		if gets.Add(1) == 1 {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("response writer cannot hijack")
				return
			}
			conn, buf, err := hj.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n", len(payload))
			_, _ = buf.Write(payload[:half])
			_ = buf.Flush()
			return
		}
		rng := r.Header.Get("Range")
		var start int
		_, _ = fmt.Sscanf(rng, "bytes=%d-", &start)
		if start != half {
			t.Errorf("resume Range = %q, want bytes=%d-", rng, half)
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(payload)-1, len(payload)))
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)-start))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start:])
	}))
	defer srv.Close()
	sums := testSumServer(t, payload)
	if _, err := Fetch(testFetchRef(srv.URL, sums.URL), t.TempDir(), io.Discard); err != nil {
		t.Fatal(err)
	}
	if gets.Load() != 2 {
		t.Fatalf("GETs = %d, want 2 (interrupted then resumed)", gets.Load())
	}
}

func TestFetchReusesCachedImageOffline(t *testing.T) {
	payload := []byte("verified-cache")
	var imageGets atomic.Int32
	img := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			imageGets.Add(1)
		}
		_, _ = w.Write(payload)
	}))
	defer img.Close()
	var failSums atomic.Bool
	sums := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failSums.Load() {
			http.Error(w, "offline", http.StatusBadGateway)
			return
		}
		sum := sha256.Sum256(payload)
		_, _ = fmt.Fprintf(w, "%s  ubuntu-24.04-arm64.qcow2\n", hex.EncodeToString(sum[:]))
	}))
	defer sums.Close()

	dir := t.TempDir()
	ref := testFetchRef(img.URL, sums.URL)
	if _, err := Fetch(ref, dir, io.Discard); err != nil {
		t.Fatal(err)
	}
	failSums.Store(true)
	if _, err := Fetch(ref, dir, io.Discard); err != nil {
		t.Fatalf("cached verified image must be reusable when sums endpoint is down: %v", err)
	}
	if imageGets.Load() != 1 {
		t.Fatalf("image GETs = %d, want 1", imageGets.Load())
	}
}
