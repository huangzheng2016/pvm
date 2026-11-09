package cli

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestQEMUUpgradeActivateRestore(t *testing.T) {
	root := t.TempDir()
	u := &qemuRuntimeUpgrade{
		arch:            "x86_64",
		stageRoot:       filepath.Join(root, "stage"),
		staged:          filepath.Join(root, "stage", "x86_64"),
		current:         filepath.Join(root, "qemu", "x86_64"),
		backup:          filepath.Join(root, "qemu", "x86_64.previous"),
		previousVersion: "old",
	}
	if err := os.MkdirAll(u.staged, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(u.current, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(u.staged, "new"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(u.current, "old"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(u.current, "VERSION"), []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	u.hadPrevious = true
	if err := u.Activate(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(u.current, "new")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(u.backup, "old")); err != nil {
		t.Fatal(err)
	}
	if err := u.Restore(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(u.current, "old")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(u.current, "new")); !os.IsNotExist(err) {
		t.Fatalf("new runtime survived rollback: %v", err)
	}
}

func TestQEMUUpgradeRestoreWithoutPrevious(t *testing.T) {
	u := &qemuRuntimeUpgrade{current: filepath.Join(t.TempDir(), "x86_64")}
	if err := os.MkdirAll(u.current, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := u.Restore(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(u.current); !os.IsNotExist(err) {
		t.Fatalf("new runtime was not removed: %v", err)
	}
}

func TestExtractQEMUArchive(t *testing.T) {
	arc := filepath.Join(t.TempDir(), "runtime.tar.gz")
	f, err := os.Create(arc)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "x86_64/bin/qemu-system-x86_64", Mode: 0o755, Size: 2, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("#!")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "out")
	if err := extractQEMUArchive(arc, dst); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dst, "x86_64", "bin", "qemu-system-x86_64"))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, err = %v", fi.Mode().Perm(), err)
	}
}

func TestExtractQEMUArchiveRejectsTraversal(t *testing.T) {
	arc := filepath.Join(t.TempDir(), "runtime.tar.gz")
	f, err := os.Create(arc)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := extractQEMUArchive(arc, filepath.Join(t.TempDir(), "out")); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestPrepareQEMUUpgradeSkip(t *testing.T) {
	t.Setenv("PVM_SKIP_QEMU", "1")
	u, err := prepareQEMUUpgrade(nil, "v1.2.3", t.TempDir(), "abc")
	if err != nil || u != nil {
		t.Fatalf("u=%v err=%v", u, err)
	}
}

func TestQEMUUpgradeRestoreIsIdempotent(t *testing.T) {
	root := t.TempDir()
	u := &qemuRuntimeUpgrade{
		current:         filepath.Join(root, "qemu", "x86_64"),
		backup:          filepath.Join(root, "qemu", "x86_64.previous"),
		hadPrevious:     true,
		previousVersion: "old",
	}
	if err := os.MkdirAll(u.current, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(u.current, "VERSION"), []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := u.Restore(); err != nil {
		t.Fatalf("restored runtime should not require a second backup: %v", err)
	}
}

func TestQEMUUpgradeRestoreMissingBackupPreservesCurrent(t *testing.T) {
	root := t.TempDir()
	u := &qemuRuntimeUpgrade{
		current:         filepath.Join(root, "qemu", "x86_64"),
		backup:          filepath.Join(root, "qemu", "x86_64.previous"),
		hadPrevious:     true,
		previousVersion: "old",
	}
	if err := os.MkdirAll(u.current, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(u.current, "VERSION"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := u.Restore(); err == nil {
		t.Fatal("missing backup must fail instead of deleting current runtime")
	}
	if _, err := os.Stat(filepath.Join(u.current, "VERSION")); err != nil {
		t.Fatalf("current runtime was removed: %v", err)
	}
}

func TestRestoreQEMUUpgradeUsesRecordedCurrent(t *testing.T) {
	root := t.TempDir()
	recorded := filepath.Join(root, "recorded", "x86_64")
	backup := recorded + ".previous"
	other := filepath.Join(root, "other", "x86_64")
	if err := os.MkdirAll(recorded, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(backup, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backup, "VERSION"), []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := upgradeRecovery{
		QEMUArch:        "x86_64",
		QEMUCurrent:     recorded,
		QEMUBackup:      backup,
		QEMUHadPrevious: true,
		QEMUVersion:     "old",
	}
	if err := restoreQEMUUpgrade(state); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(other, "VERSION")); !os.IsNotExist(err) {
		t.Fatalf("unrelated state directory was touched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(recorded, "VERSION")); err != nil {
		t.Fatalf("recorded runtime not restored: %v", err)
	}
}
