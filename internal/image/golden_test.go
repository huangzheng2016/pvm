package image

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/huangzheng2016/pvm/internal/native"
)

func TestGoldenVersionIncrementAndLatest(t *testing.T) {
	dir := t.TempDir()
	mk := func(e GoldenEntry) {
		cat, err := loadGoldenCatalog(dir)
		if err != nil {
			t.Fatal(err)
		}
		cat.Entries = append(cat.Entries, e)
		if err := cat.save(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(GoldenDir(dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(GoldenDir(dir), e.File), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if g := LatestGolden(dir, "ubuntu", "24.04"); g != nil {
		t.Fatalf("expected nil, got %+v", g)
	}
	mk(GoldenEntry{Version: 1, Distro: "ubuntu", ImageVersion: "24.04", File: goldenFileName("ubuntu", "24.04", 1)})
	mk(GoldenEntry{Version: 2, Distro: "ubuntu", ImageVersion: "24.04", File: goldenFileName("ubuntu", "24.04", 2)})

	g := LatestGolden(dir, "ubuntu", "24.04")
	if g == nil || g.Version != 2 {
		t.Fatalf("want v2, got %+v", g)
	}
	cat, _ := loadGoldenCatalog(dir)
	next := 1
	if ms := cat.match("ubuntu", "24.04"); len(ms) > 0 {
		next = ms[0].Version + 1
	}
	if next != 3 {
		t.Fatalf("next version want 3, got %d", next)
	}
}

func TestLatestGoldenSkipsMissingFile(t *testing.T) {
	dir := t.TempDir()
	cat := &goldenCatalog{Entries: []GoldenEntry{
		{Version: 2, Distro: "ubuntu", ImageVersion: "24.04", File: goldenFileName("ubuntu", "24.04", 2)},
		{Version: 1, Distro: "ubuntu", ImageVersion: "24.04", File: goldenFileName("ubuntu", "24.04", 1)},
	}}
	if err := os.MkdirAll(GoldenDir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := cat.save(dir); err != nil {
		t.Fatal(err)
	}
	// only v1 exists on disk
	os.WriteFile(filepath.Join(GoldenDir(dir), goldenFileName("ubuntu", "24.04", 1)), []byte("x"), 0o644)
	g := LatestGolden(dir, "ubuntu", "24.04")
	if g == nil || g.Version != 1 {
		t.Fatalf("want fallback to v1, got %+v", g)
	}
}

func TestRenderProvision(t *testing.T) {
	s, err := renderProvision("ubuntu", "baker", ProxyConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"apt-get -y install docker.io qemu-guest-agent",
		"tigervnc-standalone-server",
		"novnc websockify",
		"cloud-init clean",
		"fonts-noto-cjk",
		"ibus-libpinyin",
		"/etc/environment.d/ibus.conf",
		"/etc/skel/.config/autostart/ibus.desktop",
		"/etc/skel/.config/xfce4/xfconf/xfce-perchannel-xml/xfce4-screensaver.xml",
		"xfce4-screensaver-",
		"/etc/skel/.config/autostart/xfce4-screensaver.desktop",
		"rm -f /etc/ssh/ssh_host_*",
		"truncate -s 0 /etc/machine-id",
		"sed -i '/^pvmshare /d' /etc/fstab",
		"userdel -rf baker",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("provision script missing %q", want)
		}
	}
	if strings.Contains(s, "docker system prune -af") {
		t.Error("provision script must not nuke the docker images it just pulled")
	}
	if _, err := renderProvision("fedora", "baker", ProxyConfig{}); err == nil {
		t.Error("expected error for unsupported distro")
	}
	if _, err := renderProvision("ubuntu", "root", ProxyConfig{}); err == nil {
		t.Error("expected error for root build user")
	}
}

func TestRenderProvisionProxyLifecycle(t *testing.T) {
	proxy := ProxyConfig{
		HTTPProxy:  "http://127.0.0.1:8080",
		HTTPSProxy: "http://user:pass@localhost:8081",
		AllProxy:   "socks5://[::1]:1080",
		NoProxy:    "example.com,internal",
	}
	s, err := renderProvision("ubuntu", "baker", proxy)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"http://10.0.2.2:8080", "http://user:pass@10.0.2.2:8081", "socks5://[10.0.2.2]:1080",
		"/tmp/pvm-build-proxy.env", "/etc/systemd/system/docker.service.d/pvm-proxy.conf",
		"rm -f /tmp/pvm-build-proxy.env /etc/systemd/system/docker.service.d/pvm-proxy.conf",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("proxy provision missing %q", want)
		}
	}
	if strings.Contains(s, "localhost") {
		t.Error("loopback localhost was not rewritten")
	}
}

func TestRenderProvisionWithoutProxyHasNoProxyArtifacts(t *testing.T) {
	s, err := renderProvision("ubuntu", "baker", ProxyConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"pvm-build-proxy.env", "pvm-proxy.conf"} {
		if strings.Contains(s, forbidden) {
			t.Errorf("proxy artifact %q leaked into golden provision", forbidden)
		}
	}
}

func TestFetchUsesDefaultTransport(t *testing.T) {
	tr, ok := httpClient.Transport.(*http.Transport)
	if !ok || tr.Proxy == nil {
		t.Fatal("fetch transport must keep the Go default proxy behavior")
	}
	if tr.ResponseHeaderTimeout == 0 {
		t.Fatal("stall protection missing: response header timeout must be set")
	}
}

func TestResolveUsesNativeArchitecture(t *testing.T) {
	ref, err := Resolve("ubuntu:24.04")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Arch != native.Arch {
		t.Fatalf("Resolve arch = %q, want native %q", ref.Arch, native.Arch)
	}
}

func TestFetchSkipsAlreadyCompletePart(t *testing.T) {
	payload := []byte("already downloaded")
	gets := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			return
		}
		gets++
		_, _ = w.Write(payload)
	}))
	defer srv.Close()
	dir := t.TempDir()
	part := filepath.Join(dir, "ubuntu-24.04-arm64.qcow2.part")
	if err := os.WriteFile(part, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	sums := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(hex.EncodeToString(sum[:]) + " ubuntu-24.04-arm64.qcow2\n"))
	}))
	defer sums.Close()
	final, err := Fetch(&Ref{Distro: "ubuntu", Version: "24.04", Arch: "arm64", URL: srv.URL, SumsURL: sums.URL, SumFile: "ubuntu-24.04-arm64.qcow2", SumKind: "sha256"}, dir, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if final != filepath.Join(dir, "ubuntu-24.04-arm64.qcow2") {
		t.Fatalf("final path = %q", final)
	}
	if gets != 0 {
		t.Fatalf("complete part was downloaded again (%d GETs)", gets)
	}
}

func TestFetchRedownloadsCorruptCompletePart(t *testing.T) {
	good := []byte("known-good-image")
	gets := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", strconv.Itoa(len(good)))
			return
		}
		gets++
		_, _ = w.Write(good)
	}))
	defer srv.Close()
	sum := sha256.Sum256(good)
	sums := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(hex.EncodeToString(sum[:]) + " ubuntu-24.04-arm64.qcow2\n"))
	}))
	defer sums.Close()
	dir := t.TempDir()
	part := filepath.Join(dir, "ubuntu-24.04-arm64.qcow2.part")
	if err := os.WriteFile(part, []byte("corrupted-image!"), 0o644); err != nil {
		t.Fatal(err)
	}
	ref := &Ref{Distro: "ubuntu", Version: "24.04", Arch: "arm64", URL: srv.URL, SumsURL: sums.URL, SumFile: "ubuntu-24.04-arm64.qcow2", SumKind: "sha256"}
	if _, err := Fetch(ref, dir, io.Discard); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "ubuntu-24.04-arm64.qcow2"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(good) {
		t.Fatalf("final image = %q, want %q", got, good)
	}
	if gets != 1 {
		t.Fatalf("download count = %d, want 1", gets)
	}
}

func TestFetchKeepsCachedImageWhenChecksumServiceFails(t *testing.T) {
	cached := []byte("cached-image")
	dir := t.TempDir()
	final := filepath.Join(dir, "ubuntu-24.04-arm64.qcow2")
	if err := os.WriteFile(final, cached, 0o644); err != nil {
		t.Fatal(err)
	}
	sums := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "temporary failure", http.StatusInternalServerError)
	}))
	defer sums.Close()
	ref := &Ref{Distro: "ubuntu", Version: "24.04", Arch: "arm64", URL: "http://unused.invalid", SumsURL: sums.URL, SumFile: "ubuntu-24.04-arm64.qcow2", SumKind: "sha256"}
	if _, err := Fetch(ref, dir, io.Discard); err == nil {
		t.Fatal("expected checksum service error")
	}
	got, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(cached) {
		t.Fatalf("cached image changed to %q", got)
	}
}
