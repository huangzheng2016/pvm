package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestUpgradeRestoresAllVMsAfterFailure(t *testing.T) {
	var got []string
	err := restoreUpgradeVMs([]string{"dev1", "dev2", "dev3"}, func() int { return 0 }, func(name string) int {
		got = append(got, name)
		if name == "dev1" {
			return 1
		}
		return 0
	})
	if err == nil || !strings.Contains(err.Error(), "dev1") || !reflect.DeepEqual(got, []string{"dev1", "dev2", "dev3"}) {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestUpgradeRecoveryPreservesOriginal(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "pvm")
	if err := os.WriteFile(exe, []byte("old-binary"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := saveUpgradeRecovery(exe, upgradeRecovery{Version: "v1.0.0", Running: []string{"dev1"}, DaemonRunning: true}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(exe + ".upgrade.json")
	if err != nil {
		t.Fatal(err)
	}
	var state upgradeRecovery
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(exe + ".previous-" + state.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(old)
	if string(old) != "old-binary" || state.SHA256 != hex.EncodeToString(sum[:]) || !reflect.DeepEqual(state.Running, []string{"dev1"}) {
		t.Fatal("incomplete recovery point")
	}
	if err := os.WriteFile(exe, []byte("new-binary"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := saveUpgradeRecovery(exe, upgradeRecovery{Version: "v2.0.0"}); err != nil {
		t.Fatal(err)
	}
	preserved, err := os.ReadFile(exe + ".previous-" + state.SHA256)
	if err != nil || string(preserved) != "old-binary" {
		t.Fatal("lost earlier rollback binary", err)
	}
}

func TestUpgradeRejectsNonExecutable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pvm")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho v1.0.0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := validateUpgradeBinary(path, "v1.0.0"); err == nil {
		t.Fatal("accepted shell in place of Go binary")
	}
}

func writeUpgradeArchive(t *testing.T, path string, files map[string]string) string {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	for _, err := range []error{tw.Close(), gz.Close(), f.Close()} {
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestInstallerStagesAllAssetsBeforeReplacing(t *testing.T) {
	for _, scenario := range []string{"attestation-fails", "runtime-download-fails", "runtime-checksum-fails", "binary-version-fails", "binary-platform-fails", "unsafe-archive", "success", "binary-rename-fails"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			install := filepath.Join(dir, "install")
			fixtures := filepath.Join(dir, "fixtures")
			home := filepath.Join(dir, "state")
			for _, p := range []string{bin, install, fixtures, filepath.Join(home, "runtime/qemu/aarch64")} {
				if err := os.MkdirAll(p, 0700); err != nil {
					t.Fatal(err)
				}
			}
			write := func(path, body string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(install, "pvm"), "old-binary")
			write(filepath.Join(home, "runtime/qemu/aarch64/old"), "old-runtime")
			version := "v1.2.3"
			if scenario == "binary-version-fails" {
				version = "v9.9.9"
			}
			binaryFiles := map[string]string{"pvm": "#!/bin/sh\necho " + version + "\n"}
			if scenario == "unsafe-archive" {
				binaryFiles["../escaped"] = "must not extract"
			}
			sum := writeUpgradeArchive(t, filepath.Join(fixtures, "pvm_darwin_arm64.tar.gz"), binaryFiles)
			qsum := writeUpgradeArchive(t, filepath.Join(fixtures, "pvm-qemu-runtime-aarch64.tar.gz"), map[string]string{"aarch64/bin/qemu-system-aarch64": "#!/bin/sh\nexit 0\n", "aarch64/bin/qemu-img": "#!/bin/sh\nexit 0\n", "aarch64/share/qemu/edk2-aarch64-code.fd": "firmware"})
			if scenario == "runtime-checksum-fails" {
				qsum = strings.Repeat("0", 64)
			}
			write(filepath.Join(fixtures, "SHA256SUMS"), sum+"  pvm_darwin_arm64.tar.gz\n"+qsum+"  pvm-qemu-runtime-aarch64.tar.gz\n")
			write(filepath.Join(bin, "file"), "#!/bin/sh\nif [ \"$SCENARIO\" = binary-platform-fails ]; then echo 'ELF 64-bit executable aarch64'; else echo 'Mach-O 64-bit executable arm64'; fi\n")
			write(filepath.Join(bin, "uname"), "#!/bin/sh\ncase \"$1\" in -s) echo Darwin;; -m) echo arm64;; esac\n")
			write(filepath.Join(bin, "curl"), `#!/bin/sh
url=$2
name=${url##*/}
if [ "$SCENARIO" = runtime-download-fails ] && [ "$name" = pvm-qemu-runtime-aarch64.tar.gz ]; then exit 22; fi
cp "$FIXTURES/$name" "$4"
`)
			write(filepath.Join(bin, "gh"), "#!/bin/sh\n[ \"$SCENARIO\" != attestation-fails ]\n")
			write(filepath.Join(bin, "codesign"), "#!/bin/sh\necho '<key>com.apple.security.hypervisor</key><true/>'\n")
			mv, err := exec.LookPath("mv")
			if err != nil {
				t.Fatal(err)
			}
			moveFlag := "-fh"
			if runtime.GOOS != "darwin" {
				moveFlag = "-fT"
			}
			write(filepath.Join(bin, "mv"), fmt.Sprintf(`#!/bin/sh
if [ "$SCENARIO" = binary-rename-fails ] && [ "$1" = -f ] && [ "$3" = "$INSTALL_DIR/pvm" ]; then exit 1; fi
if [ "$1" = -fh ]; then shift; exec %q %s "$@"; fi
exec %q "$@"
`, mv, moveFlag, mv))
			script, err := filepath.Abs("../../scripts/install.sh")
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("sh", script)
			cmd.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin", "INSTALL_DIR="+install, "PVM_HOME="+home, "TAG=v1.2.3", "PVM_SKIP_QEMU=0", "FIXTURES="+fixtures, "SCENARIO="+scenario)
			out, err := cmd.CombinedOutput()
			if !strings.Contains(string(out), "downloading pvm_darwin_arm64.tar.gz") {
				t.Fatalf("installer did not reach download: %v: %s", err, out)
			}
			if scenario == "success" {
				if err != nil {
					t.Fatalf("install: %v: %s", err, out)
				}
				data, _ := os.ReadFile(filepath.Join(install, "pvm"))
				if !strings.Contains(string(data), "v1.2.3") {
					t.Fatal("new binary not installed")
				}
				if _, err := os.Readlink(filepath.Join(home, "runtime/qemu/aarch64")); err != nil {
					t.Fatal("runtime was not versioned", err)
				}
				matches, _ := filepath.Glob(filepath.Join(install, "pvm.previous.*"))
				if len(matches) != 1 {
					t.Fatal("missing old binary")
				}
			} else {
				if err == nil {
					t.Fatal("expected failure")
				}
				data, _ := os.ReadFile(filepath.Join(install, "pvm"))
				if string(data) != "old-binary" {
					t.Fatal("failure changed installed binary")
				}
				data, e := os.ReadFile(filepath.Join(home, "runtime/qemu/aarch64/old"))
				if e != nil || string(data) != "old-runtime" {
					t.Fatalf("failure lost old runtime: %v", e)
				}
			}
			if _, err := os.Stat(filepath.Join(install, "escaped")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("archive escaped staging directory", err)
			}
			if _, err := os.Stat(filepath.Join(install, ".pvm-install.lock")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("stale installer lock", err)
			}
		})
	}
}

func TestReleaseAttestationPolicy(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	gh := filepath.Join(dir, "gh")
	if err := os.WriteFile(gh, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ATTEST_ARGS\"\nexit \"$ATTEST_EXIT\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("ATTEST_ARGS", log)
	t.Setenv("ATTEST_EXIT", "0")
	if err := verifyReleaseAttestation("release.tar.gz", "v1.2.3"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--repo\n" + upgradeRepo, "--signer-workflow\n" + upgradeRepo + "/.github/workflows/build.yml", "--source-ref\nrefs/tags/v1.2.3", "--deny-self-hosted-runners"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("missing policy %q", want)
		}
	}
	t.Setenv("ATTEST_EXIT", "1")
	if err := verifyReleaseAttestation("release.tar.gz", "v1.2.3"); err == nil {
		t.Fatal("ignored attestation failure")
	}
}

func TestUpgradeBinaryVersionValidation(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	binary := filepath.Join(dir, "pvm")
	if err := os.WriteFile(source, []byte("package main\nimport \"fmt\"\nfunc main(){fmt.Println(\"v1.2.3\")}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-o", binary, source)
	cmd.Env = append(os.Environ(), "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture build: %v: %s", err, out)
	}
	if err := validateUpgradeBinary(binary, "v1.2.3"); err != nil {
		t.Fatal(err)
	}
	if err := validateUpgradeBinary(binary, "v9.9.9"); err == nil {
		t.Fatal("accepted incorrect release version")
	}
	original, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveUpgradeRecovery(binary, upgradeRecovery{Version: "v1.2.3", Running: []string{"dev1"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("broken-new-binary"), 0755); err != nil {
		t.Fatal(err)
	}
	if code := rollbackUpgrade(binary, true, true); code != 0 {
		t.Fatalf("rollback failed: %d", code)
	}
	restored, err := os.ReadFile(binary)
	if err != nil || !bytes.Equal(restored, original) {
		t.Fatal("rollback did not restore original executable", err)
	}

}
