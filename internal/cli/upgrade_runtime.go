package cli

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/huangzheng2016/pvm/internal/paths"
)

type qemuRuntimeAsset struct {
	archive  string
	arch     string
	system   string
	firmware string
}

func qemuRuntimeAssetForHost() (*qemuRuntimeAsset, error) {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "darwin/arm64":
		return &qemuRuntimeAsset{archive: "pvm-qemu-runtime-aarch64.tar.gz", arch: "aarch64", system: "qemu-system-aarch64", firmware: "share/qemu/edk2-aarch64-code.fd"}, nil
	case "linux/amd64":
		return &qemuRuntimeAsset{archive: "pvm-qemu-runtime-linux-x86_64.tar.gz", arch: "x86_64", system: "qemu-system-x86_64", firmware: "share/qemu/bios-256k.bin"}, nil
	case "windows/amd64":
		return &qemuRuntimeAsset{archive: "pvm-qemu-runtime-windows-x86_64.tar.gz", arch: "x86_64", system: "qemu-system-x86_64.exe", firmware: "bin/share/bios-256k.bin"}, nil
	default:
		return nil, fmt.Errorf("no bundled QEMU runtime for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
}

type qemuRuntimeUpgrade struct {
	arch            string
	stageRoot       string
	staged          string
	current         string
	backup          string
	hadPrevious     bool
	previousVersion string
}

func prepareQEMUUpgrade(client *http.Client, tag, workDir, sha string) (*qemuRuntimeUpgrade, error) {
	if os.Getenv("PVM_SKIP_QEMU") == "1" {
		return nil, nil
	}
	asset, err := qemuRuntimeAssetForHost()
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", upgradeRepo, tag, asset.archive)
	arc := filepath.Join(workDir, "qemu-runtime.tar.gz")
	if err := downloadFile(client, url, arc); err != nil {
		return nil, fmt.Errorf("download QEMU runtime: %w", err)
	}
	if err := verifyReleaseAttestation(arc, tag); err != nil {
		return nil, fmt.Errorf("QEMU runtime provenance: %w", err)
	}
	sums := filepath.Join(workDir, "SHA256SUMS")
	if err := verifyArchiveSum(arc, sums, asset.archive); err != nil {
		return nil, fmt.Errorf("QEMU runtime checksum: %w", err)
	}
	p, err := paths.Default()
	if err != nil {
		return nil, err
	}
	runtimeRoot := filepath.Join(p.Runtime, "qemu")
	if err := os.MkdirAll(runtimeRoot, 0o700); err != nil {
		return nil, err
	}
	stageRoot, err := os.MkdirTemp(runtimeRoot, ".upgrade-qemu-*")
	if err != nil {
		return nil, err
	}
	u := &qemuRuntimeUpgrade{
		arch:      asset.arch,
		stageRoot: stageRoot,
		staged:    filepath.Join(stageRoot, asset.arch),
		current:   filepath.Join(runtimeRoot, asset.arch),
		backup:    filepath.Join(runtimeRoot, asset.arch+".previous-"+sha),
	}
	if err := extractQEMUArchive(arc, u.stageRoot); err != nil {
		os.RemoveAll(stageRoot)
		return nil, fmt.Errorf("extract QEMU runtime: %w", err)
	}
	if err := validateQEMUUpgradeRuntime(u.staged, asset); err != nil {
		os.RemoveAll(stageRoot)
		return nil, fmt.Errorf("validate QEMU runtime: %w", err)
	}
	if _, err := os.Lstat(u.backup); err == nil {
		os.RemoveAll(stageRoot)
		return nil, fmt.Errorf("QEMU rollback path already exists: %s", u.backup)
	} else if !os.IsNotExist(err) {
		os.RemoveAll(stageRoot)
		return nil, err
	}
	if _, err := os.Lstat(u.current); err == nil {
		u.hadPrevious = true
		if data, readErr := os.ReadFile(filepath.Join(u.current, "VERSION")); readErr == nil {
			u.previousVersion = strings.TrimSpace(string(data))
		} else if !os.IsNotExist(readErr) {
			os.RemoveAll(stageRoot)
			return nil, readErr
		}
	} else if !os.IsNotExist(err) {
		os.RemoveAll(stageRoot)
		return nil, err
	}
	return u, nil
}

func (u *qemuRuntimeUpgrade) Activate() error {
	if u == nil {
		return nil
	}
	if u.hadPrevious {
		if err := os.Rename(u.current, u.backup); err != nil {
			return fmt.Errorf("preserve previous QEMU runtime: %w", err)
		}
	}
	if err := os.Rename(u.staged, u.current); err != nil {
		if u.hadPrevious {
			_ = os.Rename(u.backup, u.current)
		}
		return fmt.Errorf("activate QEMU runtime: %w", err)
	}
	return nil
}

func (u *qemuRuntimeUpgrade) Restore() error {
	if u == nil {
		return nil
	}
	if u.hadPrevious {
		if _, err := os.Lstat(u.backup); err != nil {
			if os.IsNotExist(err) {
				if u.previousVersion != "" {
					if data, readErr := os.ReadFile(filepath.Join(u.current, "VERSION")); readErr == nil && strings.TrimSpace(string(data)) == u.previousVersion {
						return nil
					}
				}
				return fmt.Errorf("QEMU rollback backup %s is missing; current runtime left unchanged", u.backup)
			}
			return err
		}
		if err := os.RemoveAll(u.current); err != nil {
			return err
		}
		return os.Rename(u.backup, u.current)
	}
	if _, err := os.Lstat(u.current); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.RemoveAll(u.current)
}

func restoreQEMUUpgrade(state upgradeRecovery) error {
	if state.QEMUArch == "" {
		return nil
	}
	current := state.QEMUCurrent
	if current == "" {
		p, err := paths.Default()
		if err != nil {
			return err
		}
		current = filepath.Join(p.Runtime, "qemu", state.QEMUArch)
	}
	u := &qemuRuntimeUpgrade{
		current:         current,
		backup:          state.QEMUBackup,
		hadPrevious:     state.QEMUHadPrevious,
		previousVersion: state.QEMUVersion,
	}
	return u.Restore()
}

func extractQEMUArchive(arcPath, dst string) error {
	f, err := os.Open(arcPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(hdr.Name))
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe archive path %q", hdr.Name)
		}
		target := filepath.Join(dst, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, hdr.FileInfo().Mode().Perm())
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported archive entry type %c for %q", hdr.Typeflag, hdr.Name)
		}
	}
}

func validateQEMUUpgradeRuntime(root string, asset *qemuRuntimeAsset) error {
	for _, rel := range []string{"VERSION", asset.firmware} {
		if fi, err := os.Stat(filepath.Join(root, rel)); err != nil || fi.Size() == 0 {
			return fmt.Errorf("required runtime file %s is missing", rel)
		}
	}
	for _, name := range []string{asset.system, "qemu-img" + executableSuffix()} {
		cmd := exec.Command(filepath.Join(root, "bin", name), "--version")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s --version: %w (%s)", name, err, strings.TrimSpace(string(out)))
		}
	}
	if runtime.GOOS == "darwin" {
		system := filepath.Join(root, "bin", asset.system)
		if out, err := exec.Command("codesign", "--verify", "--strict", system).CombinedOutput(); err != nil {
			return fmt.Errorf("codesign verify: %w (%s)", err, strings.TrimSpace(string(out)))
		}
		out, err := exec.Command("codesign", "-d", "--entitlements", ":-", system).CombinedOutput()
		if err != nil || !strings.Contains(string(out), "<key>com.apple.security.hypervisor</key><true") {
			return fmt.Errorf("QEMU HVF entitlement is missing")
		}
	}
	return nil
}

func executableSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
