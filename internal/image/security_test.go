package image

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/huangzheng2016/pvm/internal/fsutil"
	"github.com/huangzheng2016/pvm/internal/paths"
)

func TestProvisionRejectsInjectedInputs(t *testing.T) {
	for _, user := range []string{"baker;id", "baker\ntrue", "--help"} {
		if _, err := renderProvision("ubuntu", user, ProxyConfig{}); err == nil {
			t.Fatalf("accepted build user %q", user)
		}
	}
	for _, value := range []string{"http://proxy.example.com\nEOF\nid", "http://proxy.example.com\rX", "x\x00"} {
		if _, err := renderProvision("ubuntu", "baker", ProxyConfig{HTTPProxy: value}); err == nil {
			t.Fatal("accepted multiline proxy")
		}
	}
}

func TestProxyShellDoesNotTraceCredentials(t *testing.T) {
	var script strings.Builder
	script.WriteString("set -euo pipefail\n")
	ProxyConfig{HTTPProxy: "http://YOUR_TOKEN@proxy.example.com:8080"}.writeProvisionEnv(&script)
	// Only run the generated shell exports in a temporary file, not the guest setup commands.
	text := script.String()
	start := strings.Index(text, "export HTTP_PROXY=")
	end := strings.Index(text, "\nEOF\n")
	cmd := exec.Command("bash", "-c", "set -euo pipefail\n"+text[start:end]+"\ntest -n \"$HTTP_PROXY\"\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("exports failed: %v", err)
	}
	if strings.Contains(string(out), "YOUR_TOKEN") {
		t.Fatal("proxy credential was traced")
	}
	if got := systemdEnv("http://x%20y.example.com"); got != "http://x%%20y.example.com" {
		t.Fatalf("systemd specifier not escaped: %q", got)
	}
	provision, err := renderProvision("ubuntu", "baker", ProxyConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(provision, "set -eux") {
		t.Fatal("provision enables credential tracing")
	}
	syntax := exec.Command("bash", "-n")
	syntax.Stdin = strings.NewReader(provision)
	if out, err := syntax.CombinedOutput(); err != nil {
		t.Fatalf("invalid generated bash: %v %s", err, out)
	}
}

func TestFetchAndBakeRejectConcurrentBaseUse(t *testing.T) {
	dir := t.TempDir()
	ref, err := Resolve("ubuntu:24.04")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ref.FileName()), nil, 0600); err != nil {
		t.Fatal(err)
	}
	lock, err := fsutil.TryLock(filepath.Join(dir, ref.FileName()+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	if _, err := Fetch(ref, dir, io.Discard); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("fetch lock: %v", err)
	}
	p := paths.New(t.TempDir())
	p.Images = dir
	if _, err := Bake(&BakeOptions{Spec: "ubuntu:24.04", P: p}); err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("bake lock: %v", err)
	}
}

func TestResolveRejectsUnsafeUbuntuVersion(t *testing.T) {
	for _, spec := range []string{"ubuntu:../../etc", "ubuntu:24.04/path", "ubuntu:", "ubuntu:24.04\n"} {
		if _, err := Resolve(spec); err == nil {
			t.Fatalf("accepted %q", spec)
		}
	}
}

func TestChecksumListSizeBound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, strings.Repeat("x", (8<<20)+1)) }))
	defer srv.Close()
	err := verifyChecksum(&Ref{SumsURL: srv.URL}, "unused")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("unbounded sums: %v", err)
	}
}

func TestPublishGoldenPreservesExistingVersion(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "v1.qcow2")
	tmp := filepath.Join(dir, "v1.tmp")
	if err := os.WriteFile(dst, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishGolden(dir, tmp, dst, &goldenCatalog{}, GoldenEntry{File: "v1.qcow2"}); err == nil {
		t.Fatal("overwrote existing golden")
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "original" {
		t.Fatal("existing golden changed")
	}
}

func TestPublishGoldenRollsBackCatalogFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(goldenCatalogPath(dir), 0700); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(GoldenDir(dir), "v1.qcow2")
	tmp := dst + ".tmp"
	if err := os.WriteFile(tmp, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishGolden(dir, tmp, dst, &goldenCatalog{}, GoldenEntry{File: "v1.qcow2"}); err == nil {
		t.Fatal("catalog error hidden")
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatal("unpublished version blocked retry")
	}
}

func TestProxyPreservesProvisionUmask(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "run"), 0700); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	ProxyConfig{HTTPProxy: "http://YOUR_TOKEN@proxy.example.com"}.writeProvisionEnv(&b)
	script := strings.ReplaceAll(b.String(), "/run/pvm-build-proxy.env", filepath.Join(dir, "run", "proxy.env"))
	script = strings.ReplaceAll(script, "/etc/systemd/system", filepath.Join(dir, "systemd"))
	cmd := exec.Command("bash", "-c", "umask 022\n"+script+"\ntouch "+filepath.Join(dir, "ordinary"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("proxy script: %v %s", err, out)
	}
	for name, mode := range map[string]os.FileMode{"ordinary": 0644, "run/proxy.env": 0600, "systemd/docker.service.d/pvm-proxy.conf": 0600} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != mode {
			t.Fatalf("%s mode %o want %o", name, fi.Mode().Perm(), mode)
		}
	}
}

func TestBuildSSHUsesOnlyDedicatedIdentity(t *testing.T) {
	args := (&sshRunner{key: "/tmp/pvm key", port: 10022, user: "baker"}).args("true")
	joined := strings.Join(args, " ")
	for _, option := range []string{"-F /dev/null", "IdentitiesOnly=yes", "IdentityAgent=none", "ForwardAgent=no"} {
		if !strings.Contains(joined, option) {
			t.Fatalf("missing SSH isolation option %s", option)
		}
	}
}
