package image

import (
	"crypto/sha512"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPrefetchScript runs pvm_prefetch against a stub apt-get that lists
// file:// URLs, covering a good download, a hash mismatch, a failed
// download, an unsupported hash type and a file already in the cache.
func TestPrefetchScript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Git Bash curl cannot read file:// URLs of MSYS paths")
	}
	for _, tool := range []string{"bash", "curl", "sha512sum", "xargs"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
	dir := t.TempDir()
	src, arch, bin := filepath.Join(dir, "src"), filepath.Join(dir, "archives"), filepath.Join(dir, "bin")
	for _, d := range []string{src, filepath.Join(arch, "partial"), bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, data string, perm os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(data), perm); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(src, "a.deb"), "good", 0o644)
	write(filepath.Join(src, "b.deb"), "bad", 0o644)
	write(filepath.Join(arch, "d.deb"), "cached", 0o644)
	sum := sha512.Sum512([]byte("good"))
	good := hex.EncodeToString(sum[:])
	zeros := strings.Repeat("0", 128)
	write(filepath.Join(bin, "apt-get"), `#!/bin/bash
case "$*" in
*upgrade*) echo "'file://`+src+`/a.deb' a.deb 4 SHA512:`+good+`"
  echo "'file://`+src+`/b.deb' b.deb 3 SHA512:`+zeros+`" ;;
*install*) echo "'file://`+src+`/missing.deb' c.deb 5 SHA512:`+zeros+`"
  echo "'file://`+src+`/a.deb' a.deb 4 SHA512:`+good+`"
  echo "'file://`+src+`/d.deb' d.deb 6 SHA512:`+zeros+`"
  echo "'file://`+src+`/a.deb' e.deb 4 MD5Sum:abc" ;;
esac
`, 0o755)
	script := strings.ReplaceAll(prefetchScript, "/var/cache/apt/archives", arch) + "\nset -euo pipefail\npvm_prefetch\n"
	cmd := exec.Command("bash", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pvm_prefetch: %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(arch, "a.deb"))
	if err != nil || string(got) != "good" {
		t.Fatalf("a.deb = %q, %v; want the verified download\n%s", got, err, out)
	}
	if got, _ := os.ReadFile(filepath.Join(arch, "d.deb")); string(got) != "cached" {
		t.Errorf("cached d.deb was replaced: %q", got)
	}
	for _, name := range []string{"b.deb", "c.deb", "e.deb"} {
		if _, err := os.Stat(filepath.Join(arch, name)); err == nil {
			t.Errorf("%s entered the cache without a verified hash", name)
		}
		if !strings.Contains(string(out), "  "+name+"\n") {
			t.Errorf("%s missing from the not-prefetched list:\n%s", name, out)
		}
	}
	if left, _ := os.ReadDir(filepath.Join(arch, "partial")); len(left) != 0 {
		t.Errorf("partial downloads left behind: %v", left)
	}
}
